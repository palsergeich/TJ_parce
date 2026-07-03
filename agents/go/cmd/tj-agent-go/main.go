// tj-agent-go — участник bake-off (Go): нормализатор техжурнала 1С → NDJSON.
//
// Два синтаксиса запуска:
//
//  1. Контракт golden-раннера (совместим с cpp_parse/count_contexts.exe):
//     tj-agent-go <input_dir> [workers] [output.jsonl] [--no-output]
//
//  2. Контракт bake-off (docs/bakeoff-protocol.md §1.1, batch-режим):
//     tj-agent-go --input <dir> --threads <N> --sink {null|file:<path>}
//     [--stats-json <path>]
//
// Формат вывода — docs/format-spec.md v1.0: NDJSON без BOM, LF-терминатор
// каждой записи. Порядок записей внутри файла = порядок событий в файле
// при любом числе потоков (жёстче KI-11). Файлы обрабатываются в порядке
// убывания размера (совместимость с эталонным exe для golden-сравнения).
//
// Exit-коды: 0 — успех; 1 — ошибка аргументов/каталога/записи вывода;
// 2 — часть входных файлов не удалось прочитать (KI-12).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tjagent/internal/parser"
)

type config struct {
	input     string
	workers   int
	output    string // путь к NDJSON; пуст при nullSink
	nullSink  bool
	statsJSON string
}

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
}

func main() { os.Exit(run(os.Args[1:])) }

func usage() {
	fmt.Fprint(os.Stderr,
		"Использование:\n"+
			"  tj-agent-go <input_dir> [workers] [output.jsonl] [--no-output]\n"+
			"  tj-agent-go --input <dir> [--threads N] [--sink null|file:<path>] [--stats-json <path>]\n")
}

func run(args []string) int {
	cfg, ok := parseArgs(args)
	if !ok {
		return 1
	}

	st, err := os.Stat(cfg.input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: директория не существует: %s\n", cfg.input)
		return 1
	}
	if !st.IsDir() {
		fmt.Fprintf(os.Stderr, "Ошибка: указанный путь не является директорией: %s\n", cfg.input)
		return 1
	}

	var s stats
	files := findLogFiles(cfg.input, &s)
	if len(files) == 0 {
		fmt.Fprintln(os.Stdout, "Не найдено .log файлов для обработки")
		writeStatsJSON(cfg, &s, 0)
		return 0
	}

	start := time.Now()

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
	// Окно допуска ограничивает число файлов «разобран, но ещё не записан»:
	// без него, пока writer ждёт медленный головной файл (сортировка по
	// убыванию размера ставит самый большой первым), воркеры разбирали бы
	// весь остальной корпус и их NDJSON-буферы копились бы в памяти без
	// ограничения (на корпусе 175 ГБ — гарантированный OOM). Допуск строго
	// в порядке files, поэтому дедлок невозможен: writer всегда ждёт файл,
	// который уже допущен и обрабатывается.
	bufs := make([][]byte, len(files))
	done := make([]chan struct{}, len(files))
	for i := range done {
		done[i] = make(chan struct{})
	}
	window := cfg.workers * 2 // ≥ workers, иначе простой; ×2 — запас на перекрытие записи
	admit := make(chan struct{}, window)
	jobs := make(chan int)
	go func() {
		for i := range files {
			admit <- struct{}{} // место в окне освобождает writer после записи
			jobs <- i
		}
		close(jobs)
	}()
	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				bufs[i] = processFile(files[i], &s)
				close(done[i])
			}
		}()
	}

	writerFailed := false
	for i := range files {
		<-done[i]
		if out != nil && !writerFailed {
			if _, err := out.Write(bufs[i]); err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка записи в файл (диск полон?): %s: %v\n", cfg.output, err)
				writerFailed = true
			}
		}
		bufs[i] = nil
		<-admit
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
	cfg := config{workers: maxInt(1, minInt(1024, numCPU()))}
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
	next := func(i int, name string) (string, bool) {
		if i+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "Ошибка: у флага %s нет значения\n", name)
			return "", false
		}
		return args[i+1], true
	}
	for i := 0; i < len(args); i++ {
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
		case "--batch-rows", "--batch-bytes", "--flush-ms":
			// Параметры батчирования не влияют на file/null-sink — принимаем и игнорируем
			if _, ok := next(i, args[i]); !ok {
				return cfg, false
			}
			i++
		case "--follow":
			fmt.Fprintln(os.Stderr, "Ошибка: --follow пока не реализован (фаза 3)")
			return cfg, false
		default:
			fmt.Fprintf(os.Stderr, "Ошибка: неизвестный флаг %s\n", args[i])
			usage()
			return cfg, false
		}
	}
	if cfg.input == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: обязателен --input <dir>")
		return cfg, false
	}
	switch {
	case sink == "":
		fmt.Fprintln(os.Stderr, "Ошибка: обязателен --sink {null|file:<path>}")
		return cfg, false
	case sink == "null":
		cfg.nullSink = true
	case strings.HasPrefix(sink, "file:"):
		cfg.output = strings.TrimPrefix(sink, "file:")
		if cfg.output == "" {
			fmt.Fprintln(os.Stderr, "Ошибка: пустой путь в --sink file:<path>")
			return cfg, false
		}
	case strings.HasPrefix(sink, "clickhouse:"):
		fmt.Fprintln(os.Stderr, "Ошибка: --sink clickhouse пока не реализован (фаза 2, e2e-серия)")
		return cfg, false
	default:
		fmt.Fprintf(os.Stderr, "Ошибка: неизвестный sink %q\n", sink)
		return cfg, false
	}
	return cfg, true
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

// processFile читает файл целиком и возвращает готовый NDJSON-буфер его событий.
func processFile(fm fileMeta, s *stats) []byte {
	data, err := os.ReadFile(fm.path)
	if err != nil {
		s.failed.Add(1)
		fmt.Fprintf(os.Stderr, "Ошибка открытия файла: %s: %v\n", fm.path, err)
		return nil
	}
	s.bytes.Add(uint64(len(data)))

	filename := filepath.Base(fm.path)
	filePath := relFilePath(fm.path)
	filenameEsc := parser.AppendEscaped(nil, []byte(filename))
	filePathEsc := parser.AppendEscaped(nil, []byte(filePath))

	buf := make([]byte, 0, len(data)+len(data)/4+4096)
	var events, skips uint64
	parser.SplitEvents(data, func(ev []byte) {
		var ok bool
		buf, ok = parser.AppendEvent(buf, ev, fm.datePrefix, filenameEsc, filePathEsc)
		if ok {
			events++
		} else {
			skips++
		}
	})
	s.events.Add(events)
	s.parseSkips.Add(skips)
	return buf
}

// relFilePath — «ровно два уровня предков»: <коллекция>\<process_pid>\<файл>.log
// (format-spec §3). Компоненты берутся из фактического пути; отсутствующий
// предок даёт пустую часть — композиция повторяет семантику fs::path::operator/.
func relFilePath(path string) string {
	parent := filepath.Dir(path)
	grandparent := filepath.Dir(parent)
	return cppJoin(cppJoin(pathFilename(grandparent), pathFilename(parent)), filepath.Base(path))
}

// pathFilename — аналог fs::path::filename(): для корня диска возвращает "".
func pathFilename(p string) string {
	b := filepath.Base(p)
	if b == "." || b == string(filepath.Separator) || strings.HasSuffix(b, ":") {
		return ""
	}
	return b
}

// cppJoin — семантика fs::path::operator/ для относительных компонентов.
func cppJoin(p, x string) string {
	if x == "" {
		if p == "" {
			return ""
		}
		if !strings.HasSuffix(p, string(filepath.Separator)) {
			return p + string(filepath.Separator)
		}
		return p
	}
	if p == "" {
		return x
	}
	if strings.HasSuffix(p, string(filepath.Separator)) {
		return p + x
	}
	return p + string(filepath.Separator) + x
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
// плюс расшифровка skips отдельными полями (приёмник обязан игнорировать незнакомые).
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
