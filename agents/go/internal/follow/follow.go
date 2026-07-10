// follow.go — оркестратор режима --follow: обнаружение файлов (poll),
// воркеры-хвостовики, реестр файлов с чекпоинтами, graceful-стоп по stop-file.
//
// Конвейер: discovery (1 горутина, WalkDir раз в poll-ms) → размеры файлов →
// воркеры (--threads N, файл закреплён за воркером — порядок событий файла
// сохраняется) → слабы строк → батчер chsink (Retry-режим) → ClickHouse →
// OnAck → продвижение committed_offset → периодический сейвер чекпоинтов.
//
// Гарантии: ноль потерь (checkpoint двигается только после подтверждения
// вставки, min-contiguous по порядку файла — ack'и приходят строго в порядке
// вставки единственного батчера); at-least-once на краше (дубли возможны на
// окне «вставлено, но чекпоинт не сохранён», окно ≤ периода сейвера).
package follow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tjagent/internal/chsink"
	"tjagent/internal/metrics"
	"tjagent/internal/wal"
)

const (
	tailReadChunk  = 1 << 20          // чанк дочитывания хвоста
	maxRoundBytes  = 32 << 20         // бюджет одного раунда файла — справедливость между файлами воркера
	slabRows       = 1024             // слаб строк воркер → батчер
	dormantAfter   = 30 * time.Second // догнан и не растёт → закрыть хэндл (переоткроется по росту)
	dormantPoll    = 2 * time.Second  // os.Stat-обход дремлющих (кэш каталога для них может лгать)
	workerTick     = 250 * time.Millisecond
	progressPeriod = 2 * time.Second
	saverPeriod    = 1 * time.Second // страховка: обычно чекпоинт пишется сразу на ack
	workerQueueCap = 4096
)

// Config — параметры запуска follow-режима (собирает main из CLI/конфига).
type Config struct {
	Input       string   // одиночный каталог (CLI-контракт bake-off)
	Inputs      []string // ≥1 каталогов (файл конфигурации); при пустом — {Input}
	Threads     int
	DSN         string
	BatchRows   int
	BatchBytes  int64
	FlushMS     int
	StateDir    string
	StopFile    string // опционален при заданном StopCh
	PollMS      int
	IdleCloseMS int
	StatsJSON   string

	// StopCh — внешний сигнал graceful-остановки (Ctrl+C, стоп службы
	// Windows): закрытие канала равнозначно появлению stop-file.
	StopCh <-chan struct{}
	// LogLevel — error | info (по умолчанию) | debug (гейт сообщений stderr;
	// реальные ошибки печатаются всегда).
	LogLevel string
	// NoSQLNorm — выключить нормализацию SQL rich-схемы (sql_norm: false;
	// см. chsink.Config.NoSQLNorm).
	NoSQLNorm bool
	// NoCtxSKDSmart — выключить правило СКД для context_line
	// (context_skd_smart: false; см. chsink.Config.NoCtxSKDSmart).
	NoCtxSKDSmart bool

	// Дисковый буфер (ADR vector §1; walglue.go). BufferType: "" | "memory" —
	// сегодняшнее поведение (чекпоинт после ack ClickHouse, буфера нет);
	// "disk" — WAL: чекпоинт после fsync, простой БД не растит память.
	BufferType     string
	BufferPath     string // пусто → <StateDir>\buffer
	BufferMaxBytes int64  // 0 → 1 ГиБ (валидация минимума — agentcfg/CLI)
	BufferFsyncMS  int    // 0 → 500 мс
}

// Уровень логирования (устанавливается Run; атомик — читают воркеры).
var logLevel atomic.Int32 // 0=error 1=info 2=debug

func levelOf(s string) int32 {
	switch s {
	case "error":
		return 0
	case "debug":
		return 2
	default:
		return 1
	}
}

// infof — операционные сообщения (старт/стоп/прогресс/резюме/усечения).
func infof(format string, a ...any) {
	if logLevel.Load() >= 1 {
		fmt.Fprintf(os.Stderr, format, a...)
	}
}

// debugf — отладочная детализация (регистрация файлов, idle-закрытия, дрёма).
func debugf(format string, a ...any) {
	if logLevel.Load() >= 2 {
		fmt.Fprintf(os.Stderr, format, a...)
	}
}

// stats — счётчики прогона (атомики: пишут воркеры, читает прогресс/итог).
type stats struct {
	events     atomic.Uint64 // эмитировано событий (включая ещё не вставленные)
	parseSkips atomic.Uint64
	bytes      atomic.Uint64 // прочитано байт хвостов
	idleCloses atomic.Uint64 // событий, закрытых по idle-таймауту
	openErrs   atomic.Uint64
	readErrs   atomic.Uint64
	truncates  atomic.Uint64
	rebinds    atomic.Uint64 // смен идентичности (пересоздание файла под тем же путём)
}

// Run — вход follow-режима. Возвращает exit-код процесса.
func Run(cfg Config) int {
	logLevel.Store(levelOf(cfg.LogLevel))
	inputs := cfg.Inputs
	if len(inputs) == 0 {
		inputs = []string{cfg.Input}
	}
	roots := make([]string, len(inputs))
	for i, in := range inputs {
		abs, err := filepath.Abs(in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: не разрешить путь %s: %v\n", in, err)
			return 1
		}
		roots[i] = abs
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: не создать --state каталог %s: %v\n", cfg.StateDir, err)
		return 1
	}
	cp := loadCheckpoints(filepath.Join(cfg.StateDir, "checkpoints.json"))
	reg := &registry{cp: cp}

	// Дисковый буфер (buffer.type=disk): открывается ДО приёмника — recovery
	// (усечение битого хвоста, уборка подтверждённых сегментов) не зависит от
	// доступности ClickHouse.
	var walW *wal.WAL
	if cfg.BufferType == "disk" {
		dir := cfg.BufferPath
		if dir == "" {
			dir = filepath.Join(cfg.StateDir, "buffer")
		}
		maxBytes := cfg.BufferMaxBytes
		if maxBytes <= 0 {
			maxBytes = 1 << 30
		}
		segBytes := int64(0)
		// ТЕСТОВЫЕ переопределения приёмочных сценариев (продуктовый минимум
		// max_bytes 256 МиБ в валидации конфига НЕ ослабляется — только явная
		// переменная окружения с громким предупреждением).
		if v := os.Getenv("TJ_BUFFER_TEST_MAX_BYTES"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 1<<20 {
				maxBytes = n
				fmt.Fprintf(os.Stderr, "[follow] ВНИМАНИЕ: ТЕСТОВЫЙ режим — TJ_BUFFER_TEST_MAX_BYTES=%d переопределяет buffer.max_bytes (не для продакшена)\n", n)
			}
		}
		if v := os.Getenv("TJ_BUFFER_TEST_SEGMENT_BYTES"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 4<<10 {
				segBytes = n
				fmt.Fprintf(os.Stderr, "[follow] ВНИМАНИЕ: ТЕСТОВЫЙ режим — TJ_BUFFER_TEST_SEGMENT_BYTES=%d переопределяет размер сегмента (не для продакшена)\n", n)
			}
		}
		var err error
		walW, err = wal.Open(wal.Config{
			Dir:          dir,
			MaxBytes:     maxBytes,
			SegmentBytes: segBytes,
			FsyncEvery:   time.Duration(cfg.BufferFsyncMS) * time.Millisecond,
			OnDurable:    reg.advanceDurable,
			Logf: func(format string, a ...any) {
				fmt.Fprintf(os.Stderr, "[follow] "+format+"\n", a...)
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: дисковый буфер: %v\n", err)
			return 1
		}
		metrics.SetBufferStatsFunc(func() (int64, int, float64) {
			b, s, oldest := walW.Stats()
			return b, s, oldest.Seconds()
		})
		defer metrics.SetBufferStatsFunc(nil)
	}

	onAck := reg.onAck
	if walW != nil {
		onAck = walAck(walW) // чекпоинты ТЖ двигает fsync буфера (OnDurable)
	}
	sink, err := chsink.Open(context.Background(), chsink.Config{
		DSN:           cfg.DSN,
		BatchRows:     cfg.BatchRows,
		BatchBytes:    cfg.BatchBytes,
		Flush:         time.Duration(cfg.FlushMS) * time.Millisecond,
		Retry:         true,
		OnAck:         onAck,
		NoSQLNorm:     cfg.NoSQLNorm,
		NoCtxSKDSmart: cfg.NoCtxSKDSmart,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: ClickHouse-sink: %v\n", err)
		return 1
	}

	st := &stats{}
	stop := make(chan struct{})
	nw := cfg.Threads
	if nw < 1 {
		nw = 1
	}
	idle := time.Duration(cfg.IdleCloseMS) * time.Millisecond
	pollDur := time.Duration(cfg.PollMS) * time.Millisecond
	workers := make([]*worker, nw)
	var wg sync.WaitGroup
	for i := range workers {
		w := &worker{
			in:        make(chan sizeMsg, workerQueueCap),
			stop:      stop,
			reg:       reg,
			sink:      sink,
			builder:   chsink.NewRowBuilder(sink.RichSchema(), sink.SQLNorm(), sink.CtxSKDSmart()),
			tailers:   map[uint32]*tailer{},
			st:        st,
			idleClose: idle,
			poll:      pollDur,
			wal:       walW,
		}
		workers[i] = w
		wg.Add(1)
		go func() { defer wg.Done(); w.run() }()
	}

	// Дренер буфера — единственный читатель WAL (walglue.go).
	drainDone := make(chan struct{})
	if walW != nil {
		builder := chsink.NewRowBuilder(sink.RichSchema(), sink.SQLNorm(), sink.CtxSKDSmart())
		go func() {
			defer close(drainDone)
			drainWAL(walW, sink, builder, stop)
		}()
	} else {
		close(drainDone)
	}

	// Сейвер чекпоинтов: атомарная перезапись раз в saverPeriod при изменениях.
	saverDone := make(chan struct{})
	go func() {
		defer close(saverDone)
		t := time.NewTicker(saverPeriod)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				reg.saveIfDirty()
			}
		}
	}()

	// Периодический прогресс — только stderr, stdout остаётся чистым.
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		t := time.NewTicker(progressPeriod)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				files, lag := reg.lag()
				bufNote := ""
				if walW != nil {
					b, s, oldest := walW.Stats()
					bufNote = fmt.Sprintf(" | буфер: %d сегм / %.1f МиБ / oldest %.0f с",
						s, float64(b)/(1<<20), oldest.Seconds())
				}
				infof("[follow] файлов: %d | событий: %d | вставлено: %d | отставание: %d Б | idle-закрытий: %d%s\n",
					files, st.events.Load(), sink.Inserted(), lag, st.idleCloses.Load(), bufNote)
			}
		}
	}()

	poll := time.Duration(cfg.PollMS) * time.Millisecond
	infof("[follow] старт: input=%s, таблица=%s, workers=%d, poll=%v, idle-close=%v, state=%s\n",
		strings.Join(roots, "; "), sink.Table(), nw, poll, idle, cfg.StateDir)

	// Причины graceful-остановки: stop-file (если задан) либо закрытие
	// внешнего канала StopCh (Ctrl+C, стоп службы Windows).
	stopReason := ""
	stopRequested := func() bool {
		if cfg.StopCh != nil {
			select {
			case <-cfg.StopCh:
				stopReason = "внешний сигнал"
				return true
			default:
			}
		}
		if cfg.StopFile != "" && fileExists(cfg.StopFile) {
			stopReason = "stop-file"
			return true
		}
		return false
	}

	// Жёсткая остановка по фатальной ошибке буфера (принцип Vector — не
	// продолжать молча): состояние восстановимо как после kill -9 — чекпоинты
	// не продвинуты за durable-данные, потерь нет.
	walFatal := func() bool {
		if walW == nil {
			return false
		}
		select {
		case <-walW.Fatal():
			return true
		default:
			return false
		}
	}
	hardStop := func() int {
		fmt.Fprintf(os.Stderr, "ОШИБКА ДИСКОВОГО БУФЕРА: %v\n", walW.FatalErr())
		fmt.Fprintln(os.Stderr, "Агент жёстко останавливается (exit 3): продолжать без исправного буфера нельзя — чекпоинты не продвинуты за durable-данные, потерь нет. Проверьте носитель/каталог буфера и перезапустите агент.")
		return walExitCode
	}

	// Цикл обнаружения: первый проход обрабатывает существующие файлы
	// (с учётом чекпоинтов), дальше — непрерывный tail. Он же следит за
	// сигналом остановки (латентность ≤ poll + время обхода; сон прерывается
	// StopCh — стоп службы не ждёт полный poll).
	d := &discovery{reg: reg, workers: workers, roots: roots, entries: map[string]*dEntry{}}
	for !stopRequested() {
		if walFatal() {
			return hardStop()
		}
		d.walk()
		if stopRequested() {
			break
		}
		if cfg.StopCh != nil {
			select {
			case <-cfg.StopCh:
			case <-time.After(poll):
			}
		} else {
			time.Sleep(poll)
		}
	}

	infof("[follow] получен сигнал остановки (%s) — graceful-останов\n", stopReason)
	sink.SetDraining() // до close(stop): воркеры/дренер могут стоять в In() на ретраях
	if walW != nil {
		walW.BeginShutdown() // отпустить воркеров, заблокированных на полном буфере
	}
	close(stop)
	wg.Wait() // воркеры: дренаж \n-терминированных pending + финальные слабы/кадры
	if walW != nil {
		// Финальный fsync: события graceful-дренажа durable, их чекпоинты
		// продвинуты. Остаток буфера доставится при следующем запуске.
		_ = walW.CloseWriter() // ошибка уже в Fatal() — проверка ниже
	}
	<-drainDone             // дренер: последний слаб отправлен
	insErr := sink.Finish() // финальный flush недобранного батча (с ретраями)
	if walW != nil {
		walW.Close()
	}
	<-saverDone
	<-progDone
	if err := reg.save(); err != nil { // финальный чекпоинт после всех ack'ов
		fmt.Fprintf(os.Stderr, "Ошибка записи чекпоинтов: %v\n", err)
	}
	if walFatal() {
		return hardStop()
	}

	files, lag := reg.lag()
	bufNote := ""
	if walW != nil {
		b, s, _ := walW.Stats()
		bufNote = fmt.Sprintf(" | остаток буфера: %d сегм / %d Б", s, b)
	}
	infof("[follow] итог: файлов: %d | событий: %d | вставлено: %d | parse_skips: %d | прочитано: %d Б | остаток (не прочитано): %d Б%s\n",
		files, st.events.Load(), sink.Inserted(), st.parseSkips.Load(), st.bytes.Load(), lag, bufNote)
	writeStatsJSON(cfg, st, reg, sink, walW)

	if insErr != nil {
		if walW != nil {
			// Неподтверждённый остаток durable в буфере: курсор не продвинут,
			// при следующем запуске он доставится — стоп успешен без потерь.
			fmt.Fprintf(os.Stderr, "[follow] финальный flush не подтверждён (%v) — остаток сохранён в дисковом буфере и будет доставлен при следующем запуске\n", insErr)
			return 0
		}
		fmt.Fprintf(os.Stderr, "ОШИБКА: финальный flush не подтверждён: %v\n", insErr)
		fmt.Fprintln(os.Stderr, "Чекпоинты не продвинуты за неподтверждённые данные — при следующем запуске они перечитаются (потерь нет)")
		return 1
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeStatsJSON — контракт bakeoff-protocol §3 (batch-поля) + follow-поля.
func writeStatsJSON(cfg Config, st *stats, reg *registry, sink *chsink.Sink, walW *wal.WAL) {
	if cfg.StatsJSON == "" {
		return
	}
	files, lag := reg.lag()
	obj := map[string]uint64{
		"events":           st.events.Load(),
		"files":            uint64(files),
		"skips":            st.parseSkips.Load(),
		"bytes":            st.bytes.Load(),
		"parse_skips":      st.parseSkips.Load(),
		"small_file_skips": 0, // гейт follow не отбрасывает файлы, а откладывает до роста
		"failed_files":     st.openErrs.Load() + st.readErrs.Load(),
		"inserted_rows":    sink.Inserted(),
		"idle_closes":      st.idleCloses.Load(),
		"truncates":        st.truncates.Load(),
		"lag_bytes":        uint64(lag),
	}
	if walW != nil {
		b, s, _ := walW.Stats()
		obj["buffer_bytes"] = uint64(b)
		obj["buffer_segments"] = uint64(s)
	}
	b, _ := json.Marshal(obj)
	if err := os.WriteFile(cfg.StatsJSON, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка записи --stats-json %s: %v\n", cfg.StatsJSON, err)
	}
}
