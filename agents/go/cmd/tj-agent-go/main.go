// tj-agent-go — агент-сборщик техжурнала 1С: нормализация → NDJSON/ClickHouse.
//
// Синтаксисы запуска:
//
//  1. Контракт golden-раннера (совместим с cpp_parse/count_contexts.exe):
//     tj-agent-go <input_dir> [workers] [output.jsonl] [--no-output]
//
//  2. Контракт bake-off (docs/bakeoff-protocol.md §1.1, batch-режим):
//     tj-agent-go --input <dir> --threads <N>
//     --sink {null|file:<path>|clickhouse[:<dsn>]}
//     [--batch-rows N] [--batch-bytes N] [--flush-ms N] [--stats-json <path>]
//
//  3. Контракт bake-off (сценарий B, follow/tail-режим):
//     tj-agent-go --follow --input <dir> --sink clickhouse[:<dsn>]
//     --state <dir> --stop-file <path>
//     [--poll-ms 500] [--idle-close-ms 2000] [--threads N] [--stats-json <path>]
//
//  4. Эксплуатационный режим (файл конфигурации, стадия 2):
//     tj-agent-go --config <tj-agent.yaml> [флаги-переопределения]
//     — follow-режим целиком из YAML (internal/agentcfg; пример —
//     agents/go/tj-agent.example.yaml); явные CLI-флаги перекрывают файл.
//     Остановка: stop-file из конфига, Ctrl+C или сигнал службы.
//
//  5. Служба Windows:
//     tj-agent-go service install|uninstall|start|stop|run --config <path> [--name <имя>]
//     — см. service_windows.go; стоп службы = graceful-дренаж follow-режима.
//
// Операционные флаги (везде): --metrics <host:port> — endpoint /metrics
// (Prometheus, по умолчанию выключен); --log-level error|info|debug;
// --log-file <path> — журнал агента вместо stderr.
//
// Формат вывода — docs/format-spec.md v1.0: NDJSON без BOM, LF-терминатор
// каждой записи. Порядок записей внутри файла = порядок событий в файле
// при любом числе потоков (жёстче KI-11). Файлы обрабатываются в порядке
// убывания размера (совместимость с эталонным exe для golden-сравнения).
//
// Exit-коды: 0 — успех; 1 — ошибка аргументов/каталога/записи вывода;
// 2 — часть входных файлов не удалось прочитать (KI-12) либо ошибки обхода
// каталогов, в том числе при нуле найденных файлов (KI-14, норматив v1.1).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tjagent/internal/agentcfg"
	"tjagent/internal/chsink"
	"tjagent/internal/follow"
	"tjagent/internal/metrics"
	"tjagent/internal/parser"
)

// Параметры конвейера (модель байтового допуска — см. комментарий в run()).
const (
	outChunkBytes           = 4 << 20   // NDJSON-чанк, передаваемый писателю
	admissionBytesPerWorker = 64 << 20  // бюджет допуска не-головных файлов, на воркера
	admissionBytesFloor     = 256 << 20 // нижняя граница бюджета
	chSlabRows              = 1024      // слаб строк воркер→батчер ClickHouse
)

// defaultCHDSN — DSN по умолчанию для --sink clickhouse без явного DSN
// (локальный tj-clickhouse, native TCP 9001, база tj_bench → таблица events).
const defaultCHDSN = "clickhouse://localhost:9001/tj_bench"

// Политика батчей ClickHouse по умолчанию (bakeoff-protocol §1.2):
// 50 000 строк ИЛИ 64 МБ ИЛИ 1000 мс — что наступит раньше.
const (
	defaultBatchRows  = 50000
	defaultBatchBytes = 64 << 20
	defaultFlushMS    = 1000
)

type config struct {
	input      string
	workers    int
	output     string // путь к NDJSON; пуст при nullSink
	nullSink   bool
	statsJSON  string
	chDSN      string // непустой → sink clickhouse (DSN может нести ?table=)
	batchRows  int
	batchBytes int64
	flushMS    int

	// follow-режим (сценарий B)
	follow      bool
	stateDir    string
	stopFile    string
	pollMS      int
	idleCloseMS int

	// эксплуатационный контур (стадия 2)
	configPath  string          // --config: YAML-файл, база значений
	inputs      []string        // каталоги ТЖ (конфиг); пусто → {input}
	metricsAddr string          // --metrics: адрес /metrics; пусто — выключен
	logLevel    string          // --log-level: error|info|debug (пусто = info)
	logFile     string          // --log-file: журнал вместо stderr
	noSQLNorm   bool            // sql_norm: false в конфиге (CLI-флага нет)
	ctxSKDSmart bool            // --context-skd-smart / context_skd_smart: правило СКД context_line (по умолчанию включено)
	stopCh      chan struct{}   // внешний стоп (служба Windows); nil → Ctrl+C
	seen        map[string]bool // явно заданные CLI-флаги (слияние с конфигом)
}

// Значения по умолчанию follow-контракта.
const (
	defaultPollMS      = 500
	defaultIdleCloseMS = 2000
)

type fileMeta struct {
	path       string
	size       int64
	datePrefix string
}

type stats struct {
	events     atomic.Uint64
	parseSkips atomic.Uint64
	smallSkips atomic.Uint64
	failed     atomic.Uint64
	bytes      atomic.Uint64
	inserted   atomic.Uint64 // строки, подтверждённые ClickHouse (только CH-sink)
}

func main() { os.Exit(run(os.Args[1:])) }

func usage() {
	fmt.Fprint(os.Stderr,
		"Использование:\n"+
			"  tj-agent-go <input_dir> [workers] [output.jsonl] [--no-output]\n"+
			"  tj-agent-go --input <dir> [--threads N] [--sink null|file:<path>|clickhouse[:<dsn>]]\n"+
			"              [--batch-rows N] [--batch-bytes N] [--flush-ms N] [--stats-json <path>]\n"+
			"  tj-agent-go --follow --input <dir> --sink clickhouse[:<dsn>] --state <dir> --stop-file <path>\n"+
			"              [--poll-ms 500] [--idle-close-ms 2000] [--threads N] [--stats-json <path>]\n"+
			"  tj-agent-go --config <tj-agent.yaml> [флаги-переопределения]\n"+
			"  tj-agent-go service install|uninstall|start|stop|run --config <path> [--name <имя службы>]\n"+
			"Операционные флаги: --metrics <host:port> | --log-level error|info|debug | --log-file <path>\n"+
			"                    --context-skd-smart true|false (правило СКД для context_line rich-схемы, по умолчанию true)\n")
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "service" {
		return serviceCommand(args[1:])
	}
	cfg, ok := parseArgs(args)
	if !ok {
		return 1
	}
	if cfg.configPath != "" {
		if cfg, ok = applyConfigFile(cfg); !ok {
			return 1
		}
	}
	if len(cfg.inputs) == 0 {
		cfg.inputs = []string{cfg.input}
	}

	for _, in := range cfg.inputs {
		st, err := os.Stat(in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: директория не существует: %s\n", in)
			return 1
		}
		if !st.IsDir() {
			fmt.Fprintf(os.Stderr, "Ошибка: указанный путь не является директорией: %s\n", in)
			return 1
		}
	}

	// Журнал агента в файл (--log-file/log_file) — до первого сообщения.
	if cfg.logFile != "" {
		closeLog, err := redirectStderr(cfg.logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: лог-файл %s: %v\n", cfg.logFile, err)
			return 1
		}
		defer closeLog()
	}
	// Endpoint /metrics (по умолчанию выключен; fail-fast на занятом порту).
	if cfg.metricsAddr != "" {
		srv, actual, err := metrics.StartServer(cfg.metricsAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: /metrics на %s: %v\n", cfg.metricsAddr, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "[metrics] endpoint: http://%s/metrics\n", actual)
		defer srv.Close()
	}

	if cfg.follow {
		stopCh := (<-chan struct{})(cfg.stopCh)
		if stopCh == nil {
			stopCh = interruptStopCh() // консоль: Ctrl+C — graceful-стоп
		}
		return follow.Run(follow.Config{
			Input:       cfg.input,
			Inputs:      cfg.inputs,
			Threads:     cfg.workers,
			DSN:         cfg.chDSN,
			BatchRows:   cfg.batchRows,
			BatchBytes:  cfg.batchBytes,
			FlushMS:     cfg.flushMS,
			StateDir:    cfg.stateDir,
			StopFile:    cfg.stopFile,
			PollMS:      cfg.pollMS,
			IdleCloseMS: cfg.idleCloseMS,
			StatsJSON:     cfg.statsJSON,
			StopCh:        stopCh,
			LogLevel:      cfg.logLevel,
			NoSQLNorm:     cfg.noSQLNorm,
			NoCtxSKDSmart: !cfg.ctxSKDSmart,
		})
	}

	var s stats
	files := findLogFiles(cfg.input, &s)
	if len(files) == 0 {
		fmt.Fprintln(os.Stdout, "Не найдено .log файлов для обработки")
		writeStatsJSON(cfg, &s, 0)
		// KI-14 (format-spec §7): ошибки перечисления каталогов считаются и
		// дают exit 2 даже при нуле найденных файлов (ранее — ложный успех 0).
		if s.failed.Load() > 0 {
			fmt.Fprintln(os.Stderr, "ВНИМАНИЕ: обход каталогов завершился с ошибками (см. счётчик ошибок)")
			return 2
		}
		return 0
	}

	start := time.Now()

	if cfg.chDSN != "" {
		return runClickHouse(cfg, files, &s, start)
	}

	// Выход открываем до разбора: пустой (но существующий) файл — валидный
	// результат, если все события отфильтрованы (как у эталонного exe).
	var out *bufio.Writer
	var outFile *os.File
	if !cfg.nullSink {
		if dir := filepath.Dir(cfg.output); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка: не удалось создать директории для файла %s: %v\n", cfg.output, err)
				return 1
			}
		}
		var err error
		outFile, err = os.Create(cfg.output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: не удалось открыть файл для записи %s: %v\n", cfg.output, err)
			return 1
		}
		out = bufio.NewWriterSize(outFile, 4<<20)
	}

	// Параллельный разбор по файлам, запись строго в порядке files
	// (детерминизм не зависит от workers — требование v1.1 формата, §5).
	//
	// Память ограничена байтовым бюджетом допуска (зеркало core/src/pipeline.cpp,
	// но допуск считается в БАЙТАХ, а не в числе файлов):
	//   - вход читается чанками (parser.ScanEvents) — файл никогда не лежит
	//     в памяти целиком, резидентность O(чанк + максимальное событие);
	//   - головной файл (i == filesWritten, чья очередь писаться) допускается
	//     без бюджета: его NDJSON-чанки писатель забирает из канала на лету,
	//     вывод не копится (стриминг);
	//   - остальные допускаются, только если их размер помещается в остаток
	//     бюджета 64 МБ × workers (минимум 256 МБ) — их вывод буферизуется
	//     в канале слота до своей очереди. Файл больше остатка ждёт, пока сам
	//     станет головой, и тогда стримится без буферизации.
	// Дедлок невозможен: допуск строго по возрастанию индекса, писатель всегда
	// ждёт файл, который либо уже допущен и разбирается, либо станет головой
	// и будет допущен без бюджета.
	budget := int64(cfg.workers) * admissionBytesPerWorker
	if budget < admissionBytesFloor {
		budget = admissionBytesFloor
	}

	slots := make([]chan []byte, len(files))
	for i, fm := range files {
		// Голове хватает короткого канала — писатель разгружает его на лету.
		// Файлу, допустимому вне головы, даём вместимость на весь его вывод
		// (оценка сверху ×8 на вырожденно коротких событиях; сами заголовки
		// канала — машинные слова, не мегабайты), чтобы воркер не блокировался.
		c := 16
		if fm.size <= budget {
			c = int(fm.size/outChunkBytes)*8 + 8
			if c > 8192 {
				c = 8192
			}
		}
		slots[i] = make(chan []byte, c)
	}

	var (
		mu           sync.Mutex
		cond         = sync.NewCond(&mu)
		nextJob      int
		filesWritten int
		budgetUsed   int64
	)
	charged := make([]int64, len(files)) // байты, списанные с бюджета при допуске

	// acquire выдаёт воркеру следующий файл строго по возрастанию индекса.
	acquire := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		for {
			if nextJob >= len(files) {
				return 0, false
			}
			i := nextJob
			if i == filesWritten {
				// Голова: стримится писателю, бюджет не тратит и не ждёт его.
				nextJob++
				return i, true
			}
			if files[i].size <= budget-budgetUsed {
				budgetUsed += files[i].size
				charged[i] = files[i].size
				nextJob++
				return i, true
			}
			cond.Wait() // писатель разбудит: бюджет вернулся или голова сдвинулась
		}
	}

	// Пул выходных чанков: писатель возвращает отданные чанки, воркеры берут —
	// без пула на скорости диска рождались бы гигабайты короткоживущего мусора.
	chunkPool := &sync.Pool{New: func() interface{} {
		return make([]byte, 0, outChunkBytes+(256<<10))
	}}

	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inBuf := make([]byte, 0, parser.ReadChunk+parser.GuardZone)
			for {
				i, ok := acquire()
				if !ok {
					return
				}
				inBuf = processFile(files[i], &s, slots[i], chunkPool, inBuf)
				close(slots[i])
			}
		}()
	}

	writerFailed := false
	for i := range files {
		for chunk := range slots[i] {
			if out != nil && !writerFailed {
				if _, err := out.Write(chunk); err != nil {
					fmt.Fprintf(os.Stderr, "Ошибка записи в файл (диск полон?): %s: %v\n", cfg.output, err)
					writerFailed = true
				}
			}
			chunkPool.Put(chunk[:0]) //nolint:staticcheck // срезы в пуле сознательно
		}
		mu.Lock()
		budgetUsed -= charged[i]
		filesWritten++ // голова сдвинулась
		mu.Unlock()
		cond.Broadcast()
	}
	wg.Wait()

	if out != nil {
		if err := out.Flush(); err != nil && !writerFailed {
			fmt.Fprintf(os.Stderr, "Ошибка записи в файл (диск полон?): %s: %v\n", cfg.output, err)
			writerFailed = true
		}
		if err := outFile.Close(); err != nil && !writerFailed {
			fmt.Fprintf(os.Stderr, "Ошибка закрытия файла %s: %v\n", cfg.output, err)
			writerFailed = true
		}
	}

	elapsed := time.Since(start)
	reportStats(cfg, &s, len(files), elapsed)
	writeStatsJSON(cfg, &s, len(files))

	if writerFailed {
		fmt.Fprintln(os.Stderr, "ОШИБКА: запись результатов не удалась, вывод неполный")
		return 1
	}
	if s.failed.Load() > 0 {
		fmt.Fprintln(os.Stderr, "ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)")
		return 2
	}
	return 0
}

func parseArgs(args []string) (config, bool) {
	cfg := config{
		workers:     maxInt(1, minInt(1024, numCPU())),
		batchRows:   defaultBatchRows,
		batchBytes:  defaultBatchBytes,
		flushMS:     defaultFlushMS,
		ctxSKDSmart: true,
	}
	if len(args) == 0 {
		usage()
		return cfg, false
	}
	if strings.HasPrefix(args[0], "--") {
		return parseFlagArgs(args, cfg)
	}

	// Позиционный контракт golden-раннера
	cfg.input = args[0]
	if len(args) >= 2 {
		w, err := strconv.Atoi(args[1])
		if err != nil || w < 1 || w > 1024 {
			fmt.Fprintln(os.Stderr, "Ошибка: workers должен быть целым числом от 1 до 1024")
			return cfg, false
		}
		cfg.workers = w
	}
	if len(args) >= 3 {
		cfg.output = args[2]
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка определения текущей директории: %v\n", err)
			return cfg, false
		}
		cfg.output = filepath.Join(cwd, "result.jsonl")
	}
	if len(args) >= 4 {
		switch args[3] {
		case "--no-output", "--no-write", "--dry-run":
			cfg.nullSink = true
			cfg.output = ""
		}
	}
	return cfg, true
}

func parseFlagArgs(args []string, cfg config) (config, bool) {
	sink := ""
	cfg.pollMS = defaultPollMS
	cfg.idleCloseMS = defaultIdleCloseMS
	cfg.seen = map[string]bool{}
	next := func(i int, name string) (string, bool) {
		if i+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "Ошибка: у флага %s нет значения\n", name)
			return "", false
		}
		return args[i+1], true
	}
	for i := 0; i < len(args); i++ {
		// Явно заданные флаги перекрывают значения файла --config
		// (значения флагов сюда не попадают: их съедает i++ своего кейса).
		cfg.seen[args[i]] = true
		switch args[i] {
		case "--input":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.input = v
			i++
		case "--threads":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			w, err := strconv.Atoi(v)
			if err != nil || w < 1 || w > 1024 {
				fmt.Fprintln(os.Stderr, "Ошибка: --threads должен быть целым числом от 1 до 1024")
				return cfg, false
			}
			cfg.workers = w
			i++
		case "--sink":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			sink = v
			i++
		case "--stats-json":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.statsJSON = v
			i++
		case "--batch-rows":
			// Параметры батчирования действуют только на ClickHouse-sink;
			// для file/null принимаются и игнорируются (контракт §1.1)
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 10_000_000 {
				fmt.Fprintln(os.Stderr, "Ошибка: --batch-rows должен быть целым числом от 1 до 10000000")
				return cfg, false
			}
			cfg.batchRows = n
			i++
		case "--batch-bytes":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || n > 1<<40 {
				fmt.Fprintln(os.Stderr, "Ошибка: --batch-bytes должен быть целым числом от 1 до 2^40")
				return cfg, false
			}
			cfg.batchBytes = n
			i++
		case "--flush-ms":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 3_600_000 {
				fmt.Fprintln(os.Stderr, "Ошибка: --flush-ms должен быть целым числом от 1 до 3600000")
				return cfg, false
			}
			cfg.flushMS = n
			i++
		case "--follow":
			cfg.follow = true
		case "--state":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.stateDir = v
			i++
		case "--stop-file":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.stopFile = v
			i++
		case "--poll-ms":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 10 || n > 60_000 {
				fmt.Fprintln(os.Stderr, "Ошибка: --poll-ms должен быть целым числом от 10 до 60000")
				return cfg, false
			}
			cfg.pollMS = n
			i++
		case "--idle-close-ms":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 100 || n > 600_000 {
				fmt.Fprintln(os.Stderr, "Ошибка: --idle-close-ms должен быть целым числом от 100 до 600000")
				return cfg, false
			}
			cfg.idleCloseMS = n
			i++
		case "--config":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.configPath = v
			i++
		case "--metrics":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.metricsAddr = v
			i++
		case "--log-level":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			switch v {
			case "error", "info", "debug":
				cfg.logLevel = v
			default:
				fmt.Fprintf(os.Stderr, "Ошибка: --log-level %q (допустимо error | info | debug)\n", v)
				return cfg, false
			}
			i++
		case "--log-file":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			cfg.logFile = v
			i++
		case "--context-skd-smart":
			v, ok := next(i, args[i])
			if !ok {
				return cfg, false
			}
			b, err := strconv.ParseBool(v)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка: --context-skd-smart %q (допустимо true | false)\n", v)
				return cfg, false
			}
			cfg.ctxSKDSmart = b
			i++
		default:
			fmt.Fprintf(os.Stderr, "Ошибка: неизвестный флаг %s\n", args[i])
			usage()
			return cfg, false
		}
	}
	if cfg.configPath != "" {
		// Режим файла конфигурации: follow-режим, недостающие параметры даст
		// файл (applyConfigFile), явные флаги перекроют его значения. Sink,
		// если задан флагом, обязан быть ClickHouse (контракт follow).
		cfg.follow = true
		if sink != "" {
			if sink != "clickhouse" && !strings.HasPrefix(sink, "clickhouse:") {
				fmt.Fprintf(os.Stderr, "Ошибка: с --config поддерживается только --sink clickhouse[:<dsn>] (получен %q)\n", sink)
				return cfg, false
			}
			cfg.chDSN = normalizeCHDSN(sink)
		}
		return cfg, true
	}
	if cfg.input == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: обязателен --input <dir>")
		return cfg, false
	}
	switch {
	case sink == "":
		fmt.Fprintln(os.Stderr, "Ошибка: обязателен --sink {null|file:<path>|clickhouse[:<dsn>]}")
		return cfg, false
	case sink == "null":
		cfg.nullSink = true
	case strings.HasPrefix(sink, "file:"):
		cfg.output = strings.TrimPrefix(sink, "file:")
		if cfg.output == "" {
			fmt.Fprintln(os.Stderr, "Ошибка: пустой путь в --sink file:<path>")
			return cfg, false
		}
	case sink == "clickhouse" || strings.HasPrefix(sink, "clickhouse:"):
		cfg.chDSN = normalizeCHDSN(sink)
	default:
		fmt.Fprintf(os.Stderr, "Ошибка: неизвестный sink %q\n", sink)
		return cfg, false
	}
	if cfg.follow {
		if cfg.chDSN == "" {
			fmt.Fprintln(os.Stderr, "Ошибка: --follow поддерживает только --sink clickhouse[:<dsn>] (контракт сценария B)")
			return cfg, false
		}
		if cfg.stateDir == "" {
			fmt.Fprintln(os.Stderr, "Ошибка: --follow требует --state <dir>")
			return cfg, false
		}
		if cfg.stopFile == "" {
			fmt.Fprintln(os.Stderr, "Ошибка: --follow требует --stop-file <path> (либо запуск через --config)")
			return cfg, false
		}
	} else if cfg.stateDir != "" || cfg.stopFile != "" {
		fmt.Fprintln(os.Stderr, "Ошибка: --state/--stop-file имеют смысл только с --follow")
		return cfg, false
	}
	return cfg, true
}

// applyConfigFile — слияние конфигурации: значения файла --config — база,
// явно заданные CLI-флаги (cfg.seen) перекрывают их. Файл валидируется
// в agentcfg.Load (диапазоны, существование каталогов, известность ключей).
func applyConfigFile(cfg config) (config, bool) {
	fc, err := agentcfg.Load(cfg.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
		return cfg, false
	}
	if !cfg.seen["--input"] {
		cfg.inputs = append([]string(nil), fc.Inputs...)
		cfg.input = cfg.inputs[0]
	}
	if !cfg.seen["--sink"] {
		cfg.chDSN = normalizeCHDSN(fc.Sink)
	}
	if !cfg.seen["--threads"] {
		cfg.workers = fc.Threads
	}
	if !cfg.seen["--state"] {
		cfg.stateDir = fc.StateDir
	}
	if !cfg.seen["--stop-file"] {
		cfg.stopFile = fc.StopFile
	}
	if !cfg.seen["--poll-ms"] {
		cfg.pollMS = fc.PollMS
	}
	if !cfg.seen["--idle-close-ms"] {
		cfg.idleCloseMS = fc.IdleCloseMS
	}
	if !cfg.seen["--flush-ms"] {
		cfg.flushMS = fc.FlushMS
	}
	if !cfg.seen["--batch-rows"] {
		cfg.batchRows = fc.BatchRows
	}
	if !cfg.seen["--batch-bytes"] {
		cfg.batchBytes = fc.BatchBytes
	}
	if !cfg.seen["--metrics"] {
		cfg.metricsAddr = fc.Metrics
	}
	if !cfg.seen["--log-level"] {
		cfg.logLevel = fc.LogLevel
	}
	if !cfg.seen["--log-file"] {
		cfg.logFile = fc.LogFile
	}
	if !cfg.seen["--stats-json"] {
		cfg.statsJSON = fc.StatsJSON
	}
	if !cfg.seen["--context-skd-smart"] {
		cfg.ctxSKDSmart = fc.ContextSKDSmart
	}
	cfg.noSQLNorm = !fc.SQLNorm // ключ только в конфиге, CLI-флага нет
	// stop_file в конфиге опционален: остановка — Ctrl+C (консоль) либо
	// сигнал SCM (служба Windows); state_dir обязателен всегда.
	if cfg.stateDir == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: не задан каталог чекпоинтов (state_dir в конфиге или --state)")
		return cfg, false
	}
	return cfg, true
}

// redirectStderr направляет журнал агента (все fmt.Fprintf(os.Stderr, ...))
// в файл: append, каталог создаётся. Возвращает функцию закрытия.
func redirectStderr(path string) (func(), error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	os.Stderr = f // fmt.Fprintf(os.Stderr, ...) разыменовывает переменную при каждом вызове
	return func() { _ = f.Close() }, nil
}

// interruptStopCh — graceful-стоп по Ctrl+C: первый сигнал закрывает канал
// (дренаж pending, финальный flush, чекпоинты), после него обработчик
// снимается — повторный Ctrl+C завершает процесс немедленно (поведение ОС).
func interruptStopCh() <-chan struct{} {
	ch := make(chan struct{})
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "[follow] получен сигнал прерывания — graceful-останов (повторный Ctrl+C — немедленный выход)")
		signal.Stop(sig)
		close(ch)
	}()
	return ch
}

// runFollowFromConfigFile — запуск follow-режима строго по файлу конфигурации
// (путь службы Windows: CLI-переопределений нет, аргументы фиксирует install).
// stopCh — сигнал остановки SCM; defaultLogToState — при пустом log_file
// писать в <state_dir>\tj-agent-go.log (stderr службы уходит в никуда).
func runFollowFromConfigFile(cfgPath string, stopCh <-chan struct{}, defaultLogToState bool) int {
	fc, err := agentcfg.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
		return 1
	}
	logFile := fc.LogFile
	if logFile == "" && defaultLogToState {
		logFile = filepath.Join(fc.StateDir, "tj-agent-go.log")
	}
	if logFile != "" {
		closeLog, err := redirectStderr(logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: лог-файл %s: %v\n", logFile, err)
			return 1
		}
		defer closeLog()
	}
	if fc.Metrics != "" {
		srv, actual, err := metrics.StartServer(fc.Metrics)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: /metrics на %s: %v\n", fc.Metrics, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "[metrics] endpoint: http://%s/metrics\n", actual)
		defer srv.Close()
	}
	return follow.Run(follow.Config{
		Input:       fc.Inputs[0],
		Inputs:      fc.Inputs,
		Threads:     fc.Threads,
		DSN:         normalizeCHDSN(fc.Sink),
		BatchRows:   fc.BatchRows,
		BatchBytes:  fc.BatchBytes,
		FlushMS:     fc.FlushMS,
		StateDir:    fc.StateDir,
		StopFile:    fc.StopFile,
		PollMS:      fc.PollMS,
		IdleCloseMS: fc.IdleCloseMS,
		StatsJSON:     fc.StatsJSON,
		StopCh:        stopCh,
		LogLevel:      fc.LogLevel,
		NoSQLNorm:     !fc.SQLNorm,
		NoCtxSKDSmart: !fc.ContextSKDSmart,
	})
}

// normalizeCHDSN приводит значение --sink clickhouse[:<dsn>] к полному DSN.
// Принимаются равнозначные написания:
//
//	clickhouse                                   → DSN по умолчанию (defaultCHDSN)
//	clickhouse://host:9001/db[?...]              → как есть
//	clickhouse:clickhouse://host:9001/db[?...]   → буквальный <dsn> из контракта
//	clickhouse:host:9001/db[?...]                → дописывается схема clickhouse://
//
// Целевая таблица настраивается query-параметром table (например
// ...?table=events_go), по умолчанию — events в базе из DSN; схема таблицы —
// query-параметром schema: bench (по умолчанию, Map-таблица bake-off) или
// rich (продуктовая tj.events, см. internal/chsink/rich.go).
func normalizeCHDSN(sink string) string {
	rest := strings.TrimPrefix(sink, "clickhouse")
	rest = strings.TrimPrefix(rest, ":")
	switch {
	case rest == "":
		return defaultCHDSN
	case strings.HasPrefix(rest, "//"):
		return "clickhouse:" + rest
	case strings.Contains(rest, "://"):
		return rest
	default:
		return "clickhouse://" + rest
	}
}

// findLogFiles — рекурсивный поиск *.log размером ≥ MinFileSize.
// Сортировка по размеру по убыванию, при равенстве — порядок обхода
// (лексикографический): совпадает с эталонным exe на golden-кейсах.
func findLogFiles(root string, s *stats) []fileMeta {
	var files []fileMeta
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка обхода директорий: %v\n", err)
			s.failed.Add(1)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".log") || strings.LastIndexByte(name, '.') == 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка чтения атрибутов %s: %v\n", path, err)
			s.failed.Add(1)
			return nil
		}
		if info.Size() < parser.MinFileSize {
			s.smallSkips.Add(1)
			return nil
		}
		files = append(files, fileMeta{path: path, size: info.Size(), datePrefix: parser.DateFromFilename(name)})
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка обхода директорий: %v\n", err)
		s.failed.Add(1)
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].size > files[j].size })
	return files
}

// runClickHouse — конвейер --sink clickhouse (сценарий A, batch-ingest).
//
// В отличие от file-sink здесь нет упорядоченного писателя: батчи уходят в
// ClickHouse по мере разбора (порядок вставки между файлами не гарантируется
// и протоколом не требуется — MergeTree переупорядочивает по своему ORDER BY);
// порядок событий ОДНОГО файла в потоке строк сохраняется (файл разбирается
// одним воркером, слабы и батчи — FIFO). Память ограничена без байтового
// бюджета допуска: воркер держит O(чанк чтения + слаб), канал слабов и
// текущий батч ограничены порогами батчирования.
//
// Ошибки: недоступный сервер — фатально до начала разбора (Ping в Open);
// ошибка вставки по ходу — flush-then-fail (см. internal/chsink): воркеры
// останавливаются по Fatal(), exit 1, в статистике — только подтверждённые
// строки.
func runClickHouse(cfg config, files []fileMeta, s *stats, start time.Time) int {
	sink, err := chsink.Open(context.Background(), chsink.Config{
		DSN:           cfg.chDSN,
		BatchRows:     cfg.batchRows,
		BatchBytes:    cfg.batchBytes,
		Flush:         time.Duration(cfg.flushMS) * time.Millisecond,
		NoCtxSKDSmart: !cfg.ctxSKDSmart,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: ClickHouse-sink: %v\n", err)
		return 1
	}

	var nextFile atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cw := &chWorker{sink: sink, builder: chsink.NewRowBuilder(sink.RichSchema(), sink.SQLNorm(), sink.CtxSKDSmart())}
			inBuf := make([]byte, 0, parser.ReadChunk+parser.GuardZone)
			for {
				i := int(nextFile.Add(1)) - 1
				if i >= len(files) || cw.aborted() {
					break
				}
				inBuf = cw.processFile(files[i], s, inBuf)
			}
			cw.flush() // хвост слаба
		}()
	}
	wg.Wait()

	insErr := sink.Finish() // финальный flush недобранного батча
	s.inserted.Store(sink.Inserted())

	elapsed := time.Since(start)
	reportStats(cfg, s, len(files), elapsed)
	sec := elapsed.Seconds()
	rps := 0.0
	if sec > 0 {
		rps = float64(s.inserted.Load()) / sec
	}
	fmt.Fprintf(os.Stdout, "ClickHouse (%s): подтверждено %d строк (%.0f строк/с end-to-end)\n",
		sink.Table(), s.inserted.Load(), rps)
	writeStatsJSON(cfg, s, len(files))

	if insErr != nil {
		fmt.Fprintf(os.Stderr, "ОШИБКА: вставка в ClickHouse не удалась: %v\n", insErr)
		fmt.Fprintf(os.Stderr, "Подтверждено сервером до ошибки: %d строк; падавший батч не вставлен целиком (flush-then-fail)\n",
			s.inserted.Load())
		return 1
	}
	if s.failed.Load() > 0 {
		fmt.Fprintln(os.Stderr, "ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)")
		return 2
	}
	return 0
}

// chWorker — состояние одного воркера ClickHouse-конвейера: собственный
// RowBuilder (интернирование+scratch) и накопительный слаб строк.
type chWorker struct {
	sink    *chsink.Sink
	builder *chsink.RowBuilder
	slab    []chsink.Row
	stopped bool // получен Fatal() — производство строк прекращено
}

func (w *chWorker) aborted() bool {
	if w.stopped {
		return true
	}
	select {
	case <-w.sink.Fatal():
		w.stopped = true
		return true
	default:
		return false
	}
}

// flush отправляет накопленный слаб батчеру; false — приёмник фатально упал.
func (w *chWorker) flush() bool {
	if w.stopped {
		return false
	}
	if len(w.slab) == 0 {
		return true
	}
	select {
	case w.sink.In() <- w.slab:
		w.slab = make([]chsink.Row, 0, chSlabRows)
		return true
	case <-w.sink.Fatal():
		w.stopped = true
		return false
	}
}

// processFile — аналог processFile file-sink'а, но события конвертируются в
// строки таблицы на уровне разобранных полей (parser.ParseEventFields), без
// промежуточного NDJSON. Слаб отправляется по наполнению и в конце файла
// (строки не задерживаются у воркера — таймер flush батчера видит их сразу).
func (w *chWorker) processFile(fm fileMeta, s *stats, inBuf []byte) []byte {
	f, err := os.Open(fm.path)
	if err != nil {
		s.failed.Add(1)
		fmt.Fprintf(os.Stderr, "Ошибка открытия файла: %s: %v\n", fm.path, err)
		return inBuf
	}
	defer f.Close()

	filename := filepath.Base(fm.path)
	filePath := parser.RelFilePath(fm.path)
	if w.slab == nil {
		w.slab = make([]chsink.Row, 0, chSlabRows)
	}

	var events, skips uint64
	inBuf, bytesRead, err := parser.ScanEvents(f, inBuf, func(ev []byte) {
		if w.stopped { // после фатальной ошибки дочитываем файл вхолостую
			return
		}
		fld, ok := parser.ParseEventFields(ev)
		if !ok {
			skips++
			return
		}
		w.slab = append(w.slab, w.builder.Build(fld, fm.datePrefix, filename, filePath))
		events++
		if len(w.slab) >= chSlabRows {
			w.flush()
		}
	})
	s.bytes.Add(bytesRead)
	if err != nil {
		s.failed.Add(1)
		fmt.Fprintf(os.Stderr, "Ошибка чтения файла: %s: %v\n", fm.path, err)
	}
	w.flush()
	s.events.Add(events)
	s.parseSkips.Add(skips)
	return inBuf
}

// processFile читает файл чанками (parser.ScanEvents) и отправляет готовые
// NDJSON-чанки по ~outChunkBytes в канал слота. Файл никогда не находится
// в памяти целиком — ни входом, ни выходом. Возвращает (возможно выросший)
// входной буфер для переиспользования воркером на следующем файле.
func processFile(fm fileMeta, s *stats, out chan<- []byte, pool *sync.Pool, inBuf []byte) []byte {
	f, err := os.Open(fm.path)
	if err != nil {
		s.failed.Add(1)
		fmt.Fprintf(os.Stderr, "Ошибка открытия файла: %s: %v\n", fm.path, err)
		return inBuf
	}
	defer f.Close()

	filename := filepath.Base(fm.path)
	filePath := parser.RelFilePath(fm.path)
	filenameEsc := parser.AppendEscaped(nil, []byte(filename))
	filePathEsc := parser.AppendEscaped(nil, []byte(filePath))

	outBuf := pool.Get().([]byte)[:0]
	var events, skips uint64
	inBuf, bytesRead, err := parser.ScanEvents(f, inBuf, func(ev []byte) {
		var ok bool
		outBuf, ok = parser.AppendEvent(outBuf, ev, fm.datePrefix, filenameEsc, filePathEsc)
		if ok {
			events++
		} else {
			skips++
		}
		if len(outBuf) >= outChunkBytes {
			out <- outBuf
			outBuf = pool.Get().([]byte)[:0]
		}
	})
	s.bytes.Add(bytesRead)
	if err != nil {
		s.failed.Add(1)
		fmt.Fprintf(os.Stderr, "Ошибка чтения файла: %s: %v\n", fm.path, err)
	}
	if len(outBuf) > 0 {
		out <- outBuf
	} else {
		pool.Put(outBuf) //nolint:staticcheck
	}
	s.events.Add(events)
	s.parseSkips.Add(skips)
	return inBuf
}

func reportStats(cfg config, s *stats, nFiles int, elapsed time.Duration) {
	mb := float64(s.bytes.Load()) / (1024 * 1024)
	sec := elapsed.Seconds()
	speed := 0.0
	if sec > 0 {
		speed = mb / sec
	}
	// На успешном пути пишем сводку в stdout, как эталонный exe: golden-раннер
	// (PowerShell 5.1, $ErrorActionPreference='Stop') трактует stderr native-команды
	// под редиректом как ошибку. stderr — только для реальных ошибок.
	fmt.Fprintf(os.Stdout,
		"Файлов: %d (ошибок открытия: %d, пропущено <%d байт: %d) | Событий: %d | parse_skips: %d | %.2f МБ за %.3f с (%.1f МБ/с, workers=%d)\n",
		nFiles, s.failed.Load(), parser.MinFileSize, s.smallSkips.Load(),
		s.events.Load(), s.parseSkips.Load(), mb, sec, speed, cfg.workers)
}

// writeStatsJSON — контракт bakeoff-protocol §3: {"events":N,"files":M,"skips":K,"bytes":B}
// плюс расшифровка skips отдельными полями (приёмник обязан игнорировать
// незнакомые). Для ClickHouse-sink добавляется inserted_rows — строки,
// подтверждённые сервером (на успешном прогоне равно events).
func writeStatsJSON(cfg config, s *stats, nFiles int) {
	if cfg.statsJSON == "" {
		return
	}
	obj := map[string]uint64{
		"events":           s.events.Load(),
		"files":            uint64(nFiles),
		"skips":            s.parseSkips.Load() + s.smallSkips.Load(),
		"bytes":            s.bytes.Load(),
		"parse_skips":      s.parseSkips.Load(),
		"small_file_skips": s.smallSkips.Load(),
		"failed_files":     s.failed.Load(),
	}
	if cfg.chDSN != "" {
		obj["inserted_rows"] = s.inserted.Load()
	}
	b, _ := json.Marshal(obj)
	if err := os.WriteFile(cfg.statsJSON, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка записи --stats-json %s: %v\n", cfg.statsJSON, err)
	}
}

func numCPU() int { return runtime.NumCPU() }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
