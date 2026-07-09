// Package chsink — приёмник ClickHouse агента (batch-ingest сценария A и
// follow-режим). Протокол native TCP; горячий путь — колоночные native-блоки
// низкоуровневого клиента github.com/ClickHouse/ch-go (замер: построчный
// batch.Append clickhouse-go давал ~176 тыс. строк/с e2e, колоночные блоки —
// см. README §Скорость вставки). DSN разбирает clickhouse-go (совместимость
// синтаксиса clickhouse://user:pass@host:port/db?param=... сохранена).
//
// Схемы таблиц: bench (по умолчанию; контрактная tj_bench.events bake-off)
// и rich (продуктовая tj.events, 001_schema.sql) — выбирается query-параметром
// DSN ?schema=rich. Маппинг rich — rich.go, эталон — импортёр import-jsonl.ps1.
//
// Политика батчей (фиксирована протоколом): BatchRows строк ИЛИ BatchBytes
// байт ИЛИ Flush мс — что наступит раньше. async_insert не включается,
// серверные настройки вставки — по умолчанию.
//
// Семантика ошибок — flush-then-fail. Каждый батч отправляется синхронно:
// Do возвращается после подтверждения сервера, Inserted() учитывает только
// подтверждённые батчи. При ошибке вставки приёмник запоминает её, закрывает
// Fatal() (сигнал воркерам прекратить производство строк) и отбрасывает
// остаток потока; падавший батч не вставлен целиком — состояний
// «полубатча» не бывает, потерянных подтверждённых данных не бывает.
package chsink

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ch "github.com/ClickHouse/ch-go"
	clickhouse "github.com/ClickHouse/clickhouse-go/v2"

	"tjagent/internal/metrics"
)

// Config — параметры приёмника.
type Config struct {
	// DSN вида clickhouse://user:pass@host:port/db[?param=...]. Нестандартные
	// query-параметры (вырезаются перед передачей клиенту):
	//   table — целевая таблица (имя или db.имя), по умолчанию events в базе DSN;
	//   schema — bench (по умолчанию) | rich (продуктовая tj.events).
	DSN        string
	BatchRows  int           // порог батча по строкам (протокол: 50000)
	BatchBytes int64         // порог батча по байтам (протокол: 64 МБ)
	Flush      time.Duration // максимальный возраст непустого батча (протокол: 1000 мс)

	// Retry — режим follow: ошибка вставки не фатальна, батч повторяется с
	// экспоненциальным бэкоффом 1..30 с (строки батча удерживаются в памяти,
	// вход не читается → чтение файлов backpressure'ится). После SetDraining
	// (graceful-стоп) число дальнейших попыток ограничено, затем — фатал.
	// false — семантика batch-режима: flush-then-fail с первой ошибки.
	Retry bool
	// NoSQLNorm выключает нормализацию SQL rich-схемы (sql_norm: false в
	// конфиге агента): колонки sql_norm_hash/param_count/sql_params не
	// вычисляются и НЕ входят в INSERT — совместимость с tj.events без
	// миграции 002_sql_norm.sql. По умолчанию (false) нормализация включена.
	NoSQLNorm bool
	// OnAck вызывается после КАЖДОГО подтверждённого сервером батча со
	// строками в порядке вставки (точка продвижения чекпоинтов follow).
	// Слайс переиспользуется — удерживать за пределами вызова нельзя.
	OnAck func(rows []Row)
}

// Параметры повторов Retry-режима.
const (
	retryMinBackoff  = 1 * time.Second
	retryMaxBackoff  = 30 * time.Second
	drainMaxBackoff  = 2 * time.Second // бэкофф после SetDraining — стоп не должен длиться минуты
	drainMaxAttempts = 3               // попытки после SetDraining, затем фатал (чекпоинт не сдвинут — потерь нет)
)

// chReadTimeout — таймаут чтения одного пакета ответа сервера (дефолт ch-go
// 3 с мал для подтверждения тяжёлых блоков rich-схемы: ZSTD-кодеки + skip-индексы).
const chReadTimeout = 5 * time.Minute

// batchSenders — число параллельных INSERT-соединений batch-режима.
// Синхронная одиночная вставка простаивает на подтверждении сервера
// (применение блока ~сотни мс: парты × партиции × индексы), а воркеры в это
// время блокируются на заполненном канале слабов; пул отправителей
// перекрывает применение блоков на сервере и снимает стойло конвейера
// (замер corpus-medium: 1 соединение — 221 тыс. строк/с, пул — см. README).
// Политика батчей не меняется: каждый блок — те же 50000/64МБ/1000мс.
// Follow-режим всегда шлёт в одно соединение: чекпоинты требуют строгого
// порядка подтверждений.
const batchSenders = 3

var tableRe = regexp.MustCompile(`^[A-Za-z_][0-9A-Za-z_]*(\.[A-Za-z_][0-9A-Za-z_]*)?$`)

// Sink — соединение + горутина-батчер.
type Sink struct {
	cfg       Config
	chOpts    ch.Options
	client    *ch.Client
	insertSQL string
	table     string
	rich      bool
	sqlNorm   bool // rich && !cfg.NoSQLNorm

	in        chan []Row
	fatal     chan struct{} // закрыт при фатальной ошибке вставки
	done      chan struct{} // закрыт по завершении батчера
	draining  chan struct{} // закрыт SetDraining'ом (graceful-стоп follow)
	drainOnce sync.Once
	failOnce  sync.Once
	err       error
	inserted  atomic.Uint64
}

// Open разбирает DSN, устанавливает соединение и проверяет его Ping'ом
// (fail-fast: недоступный сервер — ошибка здесь, до начала разбора корпуса),
// затем запускает батчер.
func Open(ctx context.Context, cfg Config) (*Sink, error) {
	dsn, table, schema, err := parseSinkDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	if !tableRe.MatchString(table) {
		return nil, fmt.Errorf("недопустимое имя таблицы %q (ожидается идентификатор или db.идентификатор)", table)
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("разбор DSN ClickHouse: %w", err)
	}
	if len(opts.Addr) == 0 {
		return nil, fmt.Errorf("DSN ClickHouse без адреса: %q", dsn)
	}
	chOpts := ch.Options{
		Address:     opts.Addr[0],
		Database:    opts.Auth.Database,
		User:        opts.Auth.Username,
		Password:    opts.Auth.Password,
		DialTimeout: opts.DialTimeout,
		ReadTimeout: chReadTimeout,
		ClientName:  "tj-agent-go",
	}
	if chOpts.DialTimeout == 0 {
		chOpts.DialTimeout = 10 * time.Second
	}
	if opts.Compression != nil {
		switch opts.Compression.Method {
		case clickhouse.CompressionLZ4, clickhouse.CompressionLZ4HC:
			chOpts.Compression = ch.CompressionLZ4
		case clickhouse.CompressionZSTD:
			chOpts.Compression = ch.CompressionZSTD
		case clickhouse.CompressionNone:
			chOpts.Compression = ch.CompressionDisabled
		default:
			return nil, fmt.Errorf("сжатие %q не поддерживается native-клиентом (доступны lz4, zstd, none)",
				opts.Compression.Method)
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := ch.Dial(dialCtx, chOpts)
	if err != nil {
		return nil, fmt.Errorf("ClickHouse недоступен по адресу %s: %w", chOpts.Address, err)
	}
	if err := client.Ping(dialCtx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ClickHouse не отвечает на ping (%s): %w", chOpts.Address, err)
	}

	quoted := "`" + strings.ReplaceAll(table, ".", "`.`") + "`"
	rich := schema == "rich"
	sqlNorm := rich && !cfg.NoSQLNorm
	columns := benchInsertColumns
	if rich {
		columns = richInsertColumns(sqlNorm)
	}
	s := &Sink{
		cfg:       cfg,
		chOpts:    chOpts,
		client:    client,
		table:     table,
		rich:      rich,
		sqlNorm:   sqlNorm,
		insertSQL: "INSERT INTO " + quoted + " " + columns + " VALUES",
		in:        make(chan []Row, 32),
		fatal:     make(chan struct{}),
		done:      make(chan struct{}),
		draining:  make(chan struct{}),
	}
	// tj_ingest_queue_depth: слабы, ожидающие батчера, на момент скрейпа
	// (один действующий sink на процесс агента; Finish снимает сэмплер).
	metrics.SetQueueDepthFunc(func() int { return len(s.in) })
	if cfg.Retry {
		go s.runRetry()
	} else {
		go s.run()
	}
	return s, nil
}

// parseSinkDSN вырезает нестандартные query-параметры table и schema из DSN
// (клиент таких параметров не знает). Возвращает очищенный DSN, имя таблицы
// и схему ("bench" | "rich").
func parseSinkDSN(dsn string) (cleanDSN, table, schema string, err error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", "", "", fmt.Errorf("разбор DSN ClickHouse: %w", err)
	}
	table = "events"
	schema = "bench"
	q := u.Query()
	changed := false
	if t := q.Get("table"); t != "" {
		table = t
		q.Del("table")
		changed = true
	}
	if sc := q.Has("schema"); sc {
		v := q.Get("schema")
		switch v {
		case "bench", "rich":
			schema = v
		default:
			return "", "", "", fmt.Errorf("неизвестная схема %q в DSN (ожидается bench или rich)", v)
		}
		q.Del("schema")
		changed = true
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String(), table, schema, nil
}

// newColSet — колоночный буфер по выбранной схеме.
func (s *Sink) newColSet() colSet {
	if s.rich {
		return newRichCols(s.sqlNorm)
	}
	return newBenchCols()
}

// In — канал слабов строк. Порядок строк одного файла обязан сохраняться
// поставщиком (один файл — один воркер, слабы в порядке разбора); порядок
// между файлами не гарантируется и не требуется (README §ClickHouse-sink).
func (s *Sink) In() chan<- []Row { return s.in }

// Fatal закрывается при фатальной ошибке вставки: поставщики обязаны
// прекратить отправку (select на In()+Fatal()) и завершиться.
func (s *Sink) Fatal() <-chan struct{} { return s.fatal }

// Inserted — число строк, подтверждённых сервером (только успешные вставки).
func (s *Sink) Inserted() uint64 { return s.inserted.Load() }

// Table — целевая таблица (для сообщений/статистики).
func (s *Sink) Table() string { return s.table }

// RichSchema — true при ?schema=rich (RowBuilder обязан строить RichExt).
func (s *Sink) RichSchema() bool { return s.rich }

// SQLNorm — true, если строки обязаны нести нормализацию SQL
// (rich-схема с включённым sql_norm; флаг для NewRowBuilder).
func (s *Sink) SQLNorm() bool { return s.sqlNorm }

// SetDraining — сигнал graceful-стопа (только Retry-режим): текущие и
// последующие повторы вставки ограничиваются drainMaxAttempts попытками,
// после чего батчер фатально останавливается (несданные строки не
// чекпоинтятся — при следующем запуске перечитаются, потерь нет).
func (s *Sink) SetDraining() {
	s.drainOnce.Do(func() { close(s.draining) })
}

func (s *Sink) isDraining() bool {
	select {
	case <-s.draining:
		return true
	default:
		return false
	}
}

// Finish закрывает вход (вызывать строго после остановки всех поставщиков),
// дожидается финального flush батчера и возвращает первую фатальную ошибку.
func (s *Sink) Finish() error {
	close(s.in)
	<-s.done
	metrics.SetQueueDepthFunc(nil)
	if s.client != nil {
		_ = s.client.Close()
	}
	return s.err
}

func (s *Sink) fail(err error) {
	s.failOnce.Do(func() {
		s.err = err
		close(s.fatal)
	})
}

// sendColsOn — синхронная вставка блока: один INSERT-запрос с готовыми
// колонками. Длительность каждой попытки (успех и ошибка) — в гистограмму
// tj_ingest_insert_seconds.
func (s *Sink) sendColsOn(client *ch.Client, cols colSet) error {
	start := time.Now()
	err := client.Do(context.Background(), ch.Query{
		Body:  s.insertSQL,
		Input: cols.input(),
	})
	metrics.ObserveInsertSeconds(time.Since(start).Seconds())
	if err != nil {
		return fmt.Errorf("вставка блока (%d строк) в %s: %w", cols.rows(), s.table, err)
	}
	return nil
}

func (s *Sink) failedNow() bool {
	select {
	case <-s.fatal:
		return true
	default:
		return false
	}
}

// run — batch-режим: батчер + пул отправителей. Батчер (единственный
// получатель строк) копит колонки до порога строк/байт (по таймеру Flush —
// недобранный блок) и передаёт готовые блоки пулу из batchSenders соединений;
// пустые буферы возвращаются батчеру на переиспользование. Вставки идут
// параллельно — применение блока сервером не останавливает разбор.
//
// Ошибка вставки фатальна (flush-then-fail): Fatal() останавливает
// поставщиков, остальные блоки отбрасываются; Inserted() учитывает только
// подтверждённые блоки (уже ушедшие параллельные блоки могли подтвердиться
// после падавшего — для batch-режима это безразлично: прогон невалиден).
func (s *Sink) run() {
	defer close(s.done)

	// Пул соединений: первое установлено Open'ом, остальные доустанавливаются;
	// неудача доустановки — деградация до меньшего пула (не фатал).
	clients := []*ch.Client{s.client}
	for i := 1; i < batchSenders; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		c, err := ch.Dial(ctx, s.chOpts)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ClickHouse-sink: доступно %d из %d insert-соединений (%v)\n",
				len(clients), batchSenders, err)
			break
		}
		clients = append(clients, c)
	}
	defer func() {
		for _, c := range clients[1:] { // clients[0] == s.client, закрывает Finish
			_ = c.Close()
		}
	}()

	sendCh := make(chan colSet)             // заполненные блоки → отправители
	free := make(chan colSet, len(clients)) // пустые буферы → батчер
	for i := 0; i < len(clients); i++ {     // оборот: 1 у батчера + len(clients) в кольце
		free <- s.newColSet()
	}
	var senders sync.WaitGroup
	for _, c := range clients {
		senders.Add(1)
		go func(client *ch.Client) {
			defer senders.Done()
			for cols := range sendCh {
				if !s.failedNow() {
					if err := s.sendColsOn(client, cols); err != nil {
						metrics.BatchFailed()
						s.fail(err)
					} else {
						s.inserted.Add(uint64(cols.rows()))
						metrics.BatchOK()
						metrics.AddRows(uint64(cols.rows()))
					}
				}
				cols.reset()
				free <- cols
			}
		}(c)
	}

	cols := s.newColSet()
	var size int64
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	send := func() {
		if cols.rows() == 0 {
			return
		}
		sendCh <- cols
		cols = <-free
		size = 0
		timer.Stop()
	}

	for {
		select {
		case slab, ok := <-s.in:
			if !ok {
				send() // финальный недобранный блок
				close(sendCh)
				senders.Wait()
				return
			}
			if s.failedNow() { // после фатала слабы отбрасываются без сборки
				continue
			}
			for i := range slab {
				if cols.rows() == 0 {
					timer.Reset(s.cfg.Flush)
				}
				cols.appendRow(&slab[i])
				size += int64(slab[i].bytes)
				if cols.rows() >= s.cfg.BatchRows || size >= s.cfg.BatchBytes {
					send()
				}
			}
		case <-timer.C:
			send()
		}
	}
}

// runRetry — батчер follow-режима. Отличия от run():
//
//   - строки батча копятся в pending и удерживаются до подтверждения: при
//     ошибке вставки блок пересобирается из pending с нуля и повторяется с
//     бэкоффом retryMinBackoff..retryMaxBackoff (при разорванном соединении —
//     redial). Пока идут повторы, вход не читается — поставщики блокируются
//     на In() (чтение backpressure'ится), чекпоинты не двигаются, потерь нет.
//     Повтор после сетевой ошибки, когда сервер батч всё же применил, даёт
//     дубли — at-least-once по контракту;
//   - после SetDraining попытки ограничены drainMaxAttempts, затем фатал
//     (Fatal() освобождает заблокированных поставщиков);
//   - после каждого подтверждённого батча вызывается OnAck (продвижение
//     чекпоинтов).
func (s *Sink) runRetry() {
	defer close(s.done)

	pending := make([]Row, 0, s.cfg.BatchRows)
	cols := s.newColSet()
	var size int64
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := s.sendRetry(pending, cols); err != nil {
			return err
		}
		s.inserted.Add(uint64(len(pending)))
		metrics.AddRows(uint64(len(pending)))
		if s.cfg.OnAck != nil {
			s.cfg.OnAck(pending)
		}
		pending = pending[:0]
		size = 0
		timer.Stop()
		return nil
	}

	abort := func(err error) {
		s.fail(err)
		for range s.in { // дренаж до close(in) — поставщики не блокируются
		}
	}

	for {
		select {
		case slab, ok := <-s.in:
			if !ok {
				if err := flush(); err != nil { // финальный flush (graceful-стоп)
					s.fail(err)
				}
				return
			}
			for i := range slab {
				if len(pending) == 0 {
					timer.Reset(s.cfg.Flush)
				}
				pending = append(pending, slab[i])
				size += int64(slab[i].bytes)
				if len(pending) >= s.cfg.BatchRows || size >= s.cfg.BatchBytes {
					if err := flush(); err != nil {
						abort(err)
						return
					}
				}
			}
		case <-timer.C:
			if err := flush(); err != nil {
				abort(err)
				return
			}
		}
	}
}

// sendRetry — вставка rows с повторами. Возвращает ошибку только когда
// повторы исчерпаны (это возможно лишь после SetDraining).
//
// Метрики: каждая неудачная попытка — batches_total{status="failed"}
// (алерт rate(failed)>0 видит недоступность сервера сразу); подтверждение —
// {status="ok"} с первой попытки либо {status="retried"} после повторов.
func (s *Sink) sendRetry(rows []Row, cols colSet) error {
	backoff := retryMinBackoff
	drainTries := 0
	attempts := 0
	for {
		err := s.trySend(rows, cols)
		attempts++
		if err == nil {
			if attempts == 1 {
				metrics.BatchOK()
			} else {
				metrics.BatchRetried()
			}
			return nil
		}
		metrics.BatchFailed()
		if s.isDraining() {
			drainTries++
			if drainTries >= drainMaxAttempts {
				return fmt.Errorf("вставка %d строк в %s не удалась после %d попыток на graceful-стопе: %w",
					len(rows), s.table, drainTries, err)
			}
			if backoff > drainMaxBackoff {
				backoff = drainMaxBackoff
			}
		}
		fmt.Fprintf(os.Stderr, "[follow] вставка %d строк в %s: %v — повтор через %v\n",
			len(rows), s.table, err, backoff)
		select {
		case <-time.After(backoff):
		case <-s.draining:
			// проснуться сразу: на стопе ждать полный бэкофф не нужно
			time.Sleep(retryMinBackoff)
		}
		backoff *= 2
		if backoff > retryMaxBackoff {
			backoff = retryMaxBackoff
		}
	}
}

// trySend — одна попытка вставки: свежий блок из rows, синхронный Do.
// Разорванное/помеченное закрытым соединение переустанавливается.
func (s *Sink) trySend(rows []Row, cols colSet) error {
	if s.client == nil || s.client.IsClosed() {
		if s.client != nil {
			_ = s.client.Close()
			s.client = nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		client, err := ch.Dial(ctx, s.chOpts)
		cancel()
		if err != nil {
			return fmt.Errorf("переподключение к ClickHouse %s: %w", s.chOpts.Address, err)
		}
		s.client = client
	}
	cols.reset()
	for i := range rows {
		cols.appendRow(&rows[i])
	}
	if err := s.sendColsOn(s.client, cols); err != nil {
		// после ошибки состояние соединения неизвестно — закрыть, следующий
		// повтор начнётся с redial
		_ = s.client.Close()
		s.client = nil
		return err
	}
	return nil
}
