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
