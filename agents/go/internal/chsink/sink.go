// Package chsink — приёмник ClickHouse участника bake-off (сценарий A,
// batch-ingest). Официальный клиент clickhouse-go v2, протокол native TCP
// (bakeoff-protocol §1.2).
//
// Политика батчей (фиксирована протоколом): BatchRows строк ИЛИ BatchBytes
// байт ИЛИ Flush мс — что наступит раньше. async_insert не включается,
// серверные настройки вставки — по умолчанию.
//
// Семантика ошибок — flush-then-fail. Каждый батч отправляется синхронно:
// Send возвращается после подтверждения сервера, Inserted() учитывает только
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

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Config — параметры приёмника.
type Config struct {
	// DSN вида clickhouse://user:pass@host:port/db[?param=...]. Нестандартный
	// query-параметр table задаёт целевую таблицу (имя или db.имя) и
	// вырезается из DSN перед передачей клиенту; по умолчанию — events в базе
	// из DSN.
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
	// OnAck вызывается после КАЖДОГО подтверждённого сервером батча со
	// строками в порядке вставки (точка продвижения чекпоинтов follow).
	// Слайс переиспользуется — удерживать за пределами вызова нельзя.
	OnAck func(rows []Row)
}

// Параметры повторов Retry-режима.
const (
	retryMinBackoff   = 1 * time.Second
	retryMaxBackoff   = 30 * time.Second
	drainMaxBackoff   = 2 * time.Second // бэкофф после SetDraining — стоп не должен длиться минуты
	drainMaxAttempts  = 3               // попытки после SetDraining, затем фатал (чекпоинт не сдвинут — потерь нет)
)

var tableRe = regexp.MustCompile(`^[A-Za-z_][0-9A-Za-z_]*(\.[A-Za-z_][0-9A-Za-z_]*)?$`)

// Sink — соединение + горутина-батчер.
type Sink struct {
	cfg       Config
	conn      driver.Conn
	insertSQL string
	table     string

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
	dsn, table, err := splitTableParam(cfg.DSN)
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
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 10 * time.Second
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("подключение к ClickHouse %v: %w", opts.Addr, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ClickHouse недоступен по адресу %v: %w", opts.Addr, err)
	}

	quoted := "`" + strings.ReplaceAll(table, ".", "`.`") + "`"
	s := &Sink{
		cfg:       cfg,
		conn:      conn,
		table:     table,
		insertSQL: "INSERT INTO " + quoted + " (timestamp, duration, event, level, filename, file_path, props)",
		in:        make(chan []Row, 32),
		fatal:     make(chan struct{}),
		done:      make(chan struct{}),
		draining:  make(chan struct{}),
	}
	if cfg.Retry {
		go s.runRetry()
	} else {
		go s.run()
	}
	return s, nil
}

// splitTableParam вырезает query-параметр table из DSN (клиент такого
// параметра не знает). Возвращает очищенный DSN и имя таблицы.
func splitTableParam(dsn string) (string, string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", "", fmt.Errorf("разбор DSN ClickHouse: %w", err)
	}
	table := "events"
	q := u.Query()
	if t := q.Get("table"); t != "" {
		table = t
		q.Del("table")
		u.RawQuery = q.Encode()
	}
	return u.String(), table, nil
}

// In — канал слабов строк. Порядок строк одного файла обязан сохраняться
// поставщиком (один файл — один воркер, слабы в порядке разбора); порядок
// между файлами не гарантируется и не требуется (README §ClickHouse-sink).
func (s *Sink) In() chan<- []Row { return s.in }

// Fatal закрывается при фатальной ошибке вставки: поставщики обязаны
// прекратить отправку (select на In()+Fatal()) и завершиться.
func (s *Sink) Fatal() <-chan struct{} { return s.fatal }

// Inserted — число строк, подтверждённых сервером (только успешные Send).
func (s *Sink) Inserted() uint64 { return s.inserted.Load() }

// Table — целевая таблица (для сообщений/статистики).
func (s *Sink) Table() string { return s.table }

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
	_ = s.conn.Close()
	return s.err
}

func (s *Sink) fail(err error) {
	s.failOnce.Do(func() {
		s.err = err
		close(s.fatal)
	})
}

// run — батчер: единственный получатель строк. Копит батч до порога
// строк/байт, по таймеру Flush отправляет недобранный, по закрытию входа —
// финальный flush. Ошибка вставки фатальна: остаток потока отбрасывается
// (дренаж до закрытия канала, чтобы не подвесить поставщиков).
func (s *Sink) run() {
	defer close(s.done)
	ctx := context.Background()

	var (
		batch driver.Batch
		rows  int
		size  int64
	)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	send := func() error {
		if batch == nil || rows == 0 {
			return nil
		}
		if err := batch.Send(); err != nil {
			return fmt.Errorf("вставка батча (%d строк) в %s: %w", rows, s.table, err)
		}
		s.inserted.Add(uint64(rows))
		batch, rows, size = nil, 0, 0
		timer.Stop()
		return nil
	}

	appendRow := func(r *Row) error {
		if batch == nil {
			var err error
			if batch, err = s.conn.PrepareBatch(ctx, s.insertSQL); err != nil {
				return fmt.Errorf("подготовка батча для %s: %w", s.table, err)
			}
			timer.Reset(s.cfg.Flush)
		}
		if err := batch.Append(r.Time, r.Duration, r.Event, r.Level, r.Filename, r.FilePath, &r.Props); err != nil {
			return fmt.Errorf("добавление строки в батч %s: %w", s.table, err)
		}
		rows++
		size += int64(r.bytes)
		if rows >= s.cfg.BatchRows || size >= s.cfg.BatchBytes {
			return send()
		}
		return nil
	}

	abort := func(err error) {
		s.fail(err)
		if batch != nil {
			_ = batch.Abort()
			batch = nil
		}
		for range s.in { // дренаж до close(in) — поставщики не блокируются
		}
	}

	for {
		select {
		case slab, ok := <-s.in:
			if !ok {
				if err := send(); err != nil {
					s.fail(err)
				}
				return
			}
			for i := range slab {
				if err := appendRow(&slab[i]); err != nil {
					abort(err)
					return
				}
			}
		case <-timer.C:
			if err := send(); err != nil {
				abort(err)
				return
			}
		}
	}
}

// runRetry — батчер follow-режима. Отличия от run():
//
//   - строки батча копятся в pending и удерживаются до подтверждения: при
//     ошибке вставки батч пересобирается с нуля (PrepareBatch+Append+Send)
//     и повторяется с бэкоффом retryMinBackoff..retryMaxBackoff. Пока идут
//     повторы, вход не читается — поставщики блокируются на In() (реадинг
//     backpressure'ится), чекпоинты не двигаются, потерь нет. Повтор после
//     сетевой ошибки, когда сервер батч всё же применил, даёт дубли —
//     at-least-once по контракту;
//   - после SetDraining попытки ограничены drainMaxAttempts, затем фатал
//     (Fatal() освобождает заблокированных поставщиков);
//   - после каждого подтверждённого батча вызывается OnAck (продвижение
//     чекпоинтов).
func (s *Sink) runRetry() {
	defer close(s.done)

	pending := make([]Row, 0, s.cfg.BatchRows)
	var size int64
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := s.sendRetry(pending); err != nil {
			return err
		}
		s.inserted.Add(uint64(len(pending)))
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
func (s *Sink) sendRetry(rows []Row) error {
	backoff := retryMinBackoff
	drainTries := 0
	for {
		err := s.trySend(rows)
		if err == nil {
			return nil
		}
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

// trySend — одна попытка вставки: свежий батч, все строки, синхронный Send.
func (s *Sink) trySend(rows []Row) error {
	ctx := context.Background()
	batch, err := s.conn.PrepareBatch(ctx, s.insertSQL)
	if err != nil {
		return fmt.Errorf("подготовка батча для %s: %w", s.table, err)
	}
	for i := range rows {
		r := &rows[i]
		if err := batch.Append(r.Time, r.Duration, r.Event, r.Level, r.Filename, r.FilePath, &r.Props); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("добавление строки в батч %s: %w", s.table, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("вставка батча (%d строк) в %s: %w", len(rows), s.table, err)
	}
	return nil
}
