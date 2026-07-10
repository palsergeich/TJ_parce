package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYAML — временный конфиг с существующими каталогами.
func writeYAML(t *testing.T, lines ...string) (cfgPath, inputDir string) {
	t.Helper()
	inputDir = t.TempDir() // YAML в одинарных кавычках: обратные слэши литеральны
	content := "inputs:\n  - '" + inputDir + "'\n" +
		"sink: 'clickhouse://localhost:9001/tj?schema=rich&table=events'\n" +
		"state_dir: '" + filepath.Join(t.TempDir(), "state") + "'\n" +
		strings.Join(lines, "\n") + "\n"
	cfgPath = filepath.Join(t.TempDir(), "tj-agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, inputDir
}

// TestConfigFileBase — --config без флагов: все значения из файла,
// follow-режим включается неявно, stop-file опционален.
func TestConfigFileBase(t *testing.T) {
	cfgPath, inputDir := writeYAML(t,
		"poll_ms: 700", "idle_close_ms: 3000", "flush_ms: 250",
		"batch_rows: 1234", "batch_bytes: 999999", "threads: 3",
		"metrics: '127.0.0.1:0'", "log_level: debug")
	cfg, ok := parseArgs([]string{"--config", cfgPath})
	if !ok {
		t.Fatal("parseArgs не принял --config")
	}
	cfg, ok = applyConfigFile(cfg)
	if !ok {
		t.Fatal("applyConfigFile не принял валидный конфиг")
	}
	if !cfg.follow {
		t.Error("--config обязан включать follow-режим")
	}
	if cfg.stopFile != "" {
		t.Errorf("stop-file должен быть опционален, есть %q", cfg.stopFile)
	}
	if len(cfg.inputs) != 1 || cfg.inputs[0] != inputDir {
		t.Errorf("inputs = %v, ожидался [%s]", cfg.inputs, inputDir)
	}
	if cfg.pollMS != 700 || cfg.idleCloseMS != 3000 || cfg.flushMS != 250 ||
		cfg.batchRows != 1234 || cfg.batchBytes != 999999 || cfg.workers != 3 {
		t.Errorf("значения файла не применились: %+v", cfg)
	}
	if cfg.metricsAddr != "127.0.0.1:0" || cfg.logLevel != "debug" {
		t.Errorf("metrics/log_level не применились: %+v", cfg)
	}
	if !strings.Contains(cfg.chDSN, "clickhouse://localhost:9001/tj") ||
		!strings.Contains(cfg.chDSN, "schema=rich") {
		t.Errorf("sink из файла не применился: %q", cfg.chDSN)
	}
}

// TestCLIOverridesConfig — явные CLI-флаги перекрывают значения файла.
func TestCLIOverridesConfig(t *testing.T) {
	cfgPath, _ := writeYAML(t, "poll_ms: 700", "threads: 3", "metrics: '127.0.0.1:0'")
	otherInput := t.TempDir()
	cfg, ok := parseArgs([]string{
		"--config", cfgPath,
		"--threads", "9",
		"--input", otherInput,
		"--metrics", "127.0.0.1:1",
		"--log-level", "error",
		"--sink", "clickhouse:clickhouse://h:9001/db?schema=rich",
	})
	if !ok {
		t.Fatal("parseArgs не принял флаги")
	}
	cfg, ok = applyConfigFile(cfg)
	if !ok {
		t.Fatal("applyConfigFile упал")
	}
	if cfg.workers != 9 {
		t.Errorf("--threads не перекрыл файл: %d", cfg.workers)
	}
	if cfg.pollMS != 700 {
		t.Errorf("poll_ms из файла потерян: %d", cfg.pollMS)
	}
	if cfg.input != otherInput {
		t.Errorf("--input не перекрыл файл: %q", cfg.input)
	}
	if len(cfg.inputs) != 0 {
		// applyConfigFile не заполняет inputs при явном --input;
		// run() возьмёт {cfg.input}
		t.Errorf("inputs при явном --input: %v", cfg.inputs)
	}
	if cfg.metricsAddr != "127.0.0.1:1" || cfg.logLevel != "error" {
		t.Errorf("операционные флаги не перекрыли файл: %+v", cfg)
	}
	if cfg.chDSN != "clickhouse://h:9001/db?schema=rich" {
		t.Errorf("--sink не перекрыл файл: %q", cfg.chDSN)
	}
}

// TestContextSKDSmartPrecedence — context_skd_smart: файл задаёт значение,
// явный CLI-флаг перекрывает; по умолчанию опция включена.
func TestContextSKDSmartPrecedence(t *testing.T) {
	// По умолчанию (ни файла-ключа, ни флага) — включена
	cfgPath, _ := writeYAML(t)
	cfg, ok := parseArgs([]string{"--config", cfgPath})
	if !ok {
		t.Fatal("parseArgs")
	}
	if cfg, ok = applyConfigFile(cfg); !ok || !cfg.ctxSKDSmart {
		t.Errorf("по умолчанию context_skd_smart обязан быть true: %+v", cfg.ctxSKDSmart)
	}

	// Файл выключает
	cfgPath, _ = writeYAML(t, "context_skd_smart: false")
	cfg, ok = parseArgs([]string{"--config", cfgPath})
	if !ok {
		t.Fatal("parseArgs")
	}
	if cfg, ok = applyConfigFile(cfg); !ok || cfg.ctxSKDSmart {
		t.Error("context_skd_smart: false из файла не применился")
	}

	// Явный флаг перекрывает файл (в обе стороны)
	cfg, ok = parseArgs([]string{"--config", cfgPath, "--context-skd-smart", "true"})
	if !ok {
		t.Fatal("parseArgs с --context-skd-smart true")
	}
	if cfg, ok = applyConfigFile(cfg); !ok || !cfg.ctxSKDSmart {
		t.Error("--context-skd-smart true не перекрыл файл")
	}

	// Флаг без --config (batch-режим)
	in := t.TempDir()
	cfg, ok = parseArgs([]string{"--input", in, "--sink", "null", "--context-skd-smart", "false"})
	if !ok || cfg.ctxSKDSmart {
		t.Error("--context-skd-smart false в batch-режиме не применился")
	}

	// Мусорное значение — ошибка аргументов
	if _, ok = parseArgs([]string{"--input", in, "--sink", "null", "--context-skd-smart", "да"}); ok {
		t.Error("--context-skd-smart с не-булевым значением должен отвергаться")
	}
}

// TestConfigRejectsNonClickhouseSink — с --config допустим только
// ClickHouse-sink (контракт follow-режима).
func TestConfigRejectsNonClickhouseSink(t *testing.T) {
	cfgPath, _ := writeYAML(t)
	if _, ok := parseArgs([]string{"--config", cfgPath, "--sink", "null"}); ok {
		t.Error("--sink null с --config должен отвергаться")
	}
}

// TestFollowCLIContractUnchanged — прежний CLI-контракт (без --config)
// по-прежнему требует --stop-file/--state.
func TestFollowCLIContractUnchanged(t *testing.T) {
	in := t.TempDir()
	if _, ok := parseArgs([]string{"--follow", "--input", in, "--sink", "clickhouse"}); ok {
		t.Error("--follow без --state/--stop-file должен отвергаться")
	}
	if _, ok := parseArgs([]string{"--follow", "--input", in, "--sink", "clickhouse",
		"--state", filepath.Join(in, "st")}); ok {
		t.Error("--follow без --stop-file должен отвергаться")
	}
	if _, ok := parseArgs([]string{"--follow", "--input", in, "--sink", "clickhouse",
		"--state", filepath.Join(in, "st"), "--stop-file", filepath.Join(in, "stop")}); !ok {
		t.Error("полный follow-контракт должен приниматься")
	}
}

// TestBadConfigFails — битый конфиг валит запуск с внятной ошибкой (exit 1
// через applyConfigFile=false).
func TestBadConfigFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("inputs: []\nsink: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, ok := parseArgs([]string{"--config", p})
	if !ok {
		t.Fatal("parseArgs должен пройти (валидация в applyConfigFile)")
	}
	if _, ok = applyConfigFile(cfg); ok {
		t.Error("битый конфиг должен отвергаться")
	}
}

// TestBufferFlagsPrecedence — buffer из конфига, CLI-флаги перекрывают;
// --buffer disk без --follow — ошибка (batch-режиму WAL не нужен).
func TestBufferFlagsPrecedence(t *testing.T) {
	cfgPath, _ := writeYAML(t,
		"buffer:", "  type: disk", "  path: 'D:/buf'", "  max_bytes: 536870912", "  fsync_ms: 200")
	cfg, ok := parseArgs([]string{"--config", cfgPath})
	if !ok {
		t.Fatal("parseArgs не принял --config")
	}
	cfg, ok = applyConfigFile(cfg)
	if !ok {
		t.Fatal("applyConfigFile не принял валидный конфиг")
	}
	if cfg.bufferType != "disk" || cfg.bufferPath != "D:/buf" ||
		cfg.bufferMaxBytes != 536870912 || cfg.bufferFsyncMS != 200 {
		t.Errorf("buffer из конфига не применился: %+v", cfg)
	}

	cfg, ok = parseArgs([]string{"--config", cfgPath,
		"--buffer", "memory", "--buffer-max-bytes", "268435456", "--buffer-fsync-ms", "900"})
	if !ok {
		t.Fatal("parseArgs не принял флаги буфера")
	}
	cfg, ok = applyConfigFile(cfg)
	if !ok {
		t.Fatal("applyConfigFile не принял конфиг с переопределениями")
	}
	if cfg.bufferType != "memory" || cfg.bufferMaxBytes != 268435456 || cfg.bufferFsyncMS != 900 {
		t.Errorf("CLI-переопределения буфера не сработали: %+v", cfg)
	}
	if cfg.bufferPath != "D:/buf" {
		t.Errorf("не переопределённый buffer.path должен остаться из конфига: %q", cfg.bufferPath)
	}

	if _, ok := parseArgs([]string{"--input", t.TempDir(), "--sink", "null", "--buffer", "disk"}); ok {
		t.Error("--buffer disk без --follow обязан отклоняться")
	}
	if _, ok := parseArgs([]string{"--buffer", "floppy"}); ok {
		t.Error("--buffer floppy обязан отклоняться")
	}
	if _, ok := parseArgs([]string{"--buffer-max-bytes", "1048576"}); ok {
		t.Error("--buffer-max-bytes ниже минимума 256 МиБ обязан отклоняться")
	}
}

// TestFollowDefaultBufferMemory — follow-контракт bake-off без флагов буфера:
// buffer=memory (сегодняшнее поведение, ноль изменений).
func TestFollowDefaultBufferMemory(t *testing.T) {
	cfg, ok := parseArgs([]string{"--follow", "--input", t.TempDir(), "--sink", "clickhouse",
		"--state", t.TempDir(), "--stop-file", "s.flag"})
	if !ok {
		t.Fatal("parseArgs не принял follow-контракт")
	}
	if cfg.bufferType != "memory" {
		t.Errorf("буфер по умолчанию обязан быть memory, есть %q", cfg.bufferType)
	}
}
