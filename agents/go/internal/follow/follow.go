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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tjagent/internal/chsink"
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

	sink, err := chsink.Open(context.Background(), chsink.Config{
		DSN:           cfg.DSN,
		BatchRows:     cfg.BatchRows,
		BatchBytes:    cfg.BatchBytes,
		Flush:         time.Duration(cfg.FlushMS) * time.Millisecond,
		Retry:         true,
		OnAck:         reg.onAck,
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
		}
		workers[i] = w
		wg.Add(1)
		go func() { defer wg.Done(); w.run() }()
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
				infof("[follow] файлов: %d | событий: %d | вставлено: %d | отставание: %d Б | idle-закрытий: %d\n",
					files, st.events.Load(), sink.Inserted(), lag, st.idleCloses.Load())
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

	// Цикл обнаружения: первый проход обрабатывает существующие файлы
	// (с учётом чекпоинтов), дальше — непрерывный tail. Он же следит за
	// сигналом остановки (латентность ≤ poll + время обхода; сон прерывается
	// StopCh — стоп службы не ждёт полный poll).
	d := &discovery{reg: reg, workers: workers, roots: roots, entries: map[string]*dEntry{}}
	for !stopRequested() {
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
	sink.SetDraining() // до close(stop): воркеры могут стоять в In() на ретраях
	close(stop)
	wg.Wait()               // воркеры: дренаж \n-терминированных pending + финальные слабы
	insErr := sink.Finish() // финальный flush недобранного батча (с ретраями)
	<-saverDone
	<-progDone
	if err := reg.save(); err != nil { // финальный чекпоинт после всех ack'ов
		fmt.Fprintf(os.Stderr, "Ошибка записи чекпоинтов: %v\n", err)
	}

	files, lag := reg.lag()
	infof("[follow] итог: файлов: %d | событий: %d | вставлено: %d | parse_skips: %d | прочитано: %d Б | остаток (не прочитано): %d Б\n",
		files, st.events.Load(), sink.Inserted(), st.parseSkips.Load(), st.bytes.Load(), lag)
	writeStatsJSON(cfg, st, reg, sink)

	if insErr != nil {
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
func writeStatsJSON(cfg Config, st *stats, reg *registry, sink *chsink.Sink) {
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
	b, _ := json.Marshal(obj)
	if err := os.WriteFile(cfg.StatsJSON, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка записи --stats-json %s: %v\n", cfg.StatsJSON, err)
	}
}
