package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg — временный YAML-файл конфигурации.
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tj-agent.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// minCfg — минимальный валидный конфиг с существующим каталогом input.
func minCfg(t *testing.T, extra string) string {
	t.Helper()
	in := t.TempDir()
	return writeCfg(t,
		"inputs:\n  - '"+in+"'\n"+
			"sink: 'clickhouse://localhost:9001/tj?schema=rich'\n"+
			"state_dir: 'state'\n"+extra)
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(minCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollMS != DefaultPollMS || cfg.IdleCloseMS != DefaultIdleCloseMS ||
		cfg.FlushMS != DefaultFlushMS || cfg.BatchRows != DefaultBatchRows ||
		cfg.BatchBytes != DefaultBatchBytes {
		t.Errorf("умолчания не применились: %+v", cfg)
	}
	if cfg.Threads < 1 {
		t.Errorf("threads = %d", cfg.Threads)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, ожидался info", cfg.LogLevel)
	}
	if cfg.StopFile != "" || cfg.Metrics != "" {
		t.Errorf("опциональные поля должны быть пустыми: %+v", cfg)
	}
	if !cfg.SQLNorm {
		t.Error("sql_norm по умолчанию обязан быть включён")
	}
	if !cfg.ContextSKDSmart {
		t.Error("context_skd_smart по умолчанию обязан быть включён")
	}
}

// TestLoadSQLNormOff — ключ sql_norm: false выключает нормализацию SQL.
func TestLoadSQLNormOff(t *testing.T) {
	cfg, err := Load(minCfg(t, "sql_norm: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQLNorm {
		t.Error("sql_norm: false не применился")
	}
}

// TestLoadContextSKDSmartOff — ключ context_skd_smart: false выключает
// правило СКД для context_line (docs/context-line.md).
func TestLoadContextSKDSmartOff(t *testing.T) {
	cfg, err := Load(minCfg(t, "context_skd_smart: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextSKDSmart {
		t.Error("context_skd_smart: false не применился")
	}
}

func TestLoadFullOverride(t *testing.T) {
	cfg, err := Load(minCfg(t, strings.Join([]string{
		"stop_file: 'C:\\tmp\\stop'",
		"poll_ms: 700",
		"idle_close_ms: 3000",
		"flush_ms: 500",
		"batch_rows: 1000",
		"batch_bytes: 1048576",
		"threads: 3",
		"metrics: '0.0.0.0:9101'",
		"log_level: debug",
		"log_file: 'agent.log'",
		"stats_json: 'stats.json'",
	}, "\n")+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollMS != 700 || cfg.IdleCloseMS != 3000 || cfg.FlushMS != 500 ||
		cfg.BatchRows != 1000 || cfg.BatchBytes != 1048576 || cfg.Threads != 3 ||
		cfg.Metrics != "0.0.0.0:9101" || cfg.LogLevel != "debug" ||
		cfg.LogFile != "agent.log" || cfg.StatsJSON != "stats.json" ||
		cfg.StopFile != `C:\tmp\stop` {
		t.Errorf("значения файла не применились: %+v", cfg)
	}
}

func TestBOMTolerated(t *testing.T) {
	in := t.TempDir()
	body := "inputs:\n  - '" + in + "'\n" +
		"sink: 'clickhouse://localhost:9001/tj'\nstate_dir: s\n"
	p := filepath.Join(t.TempDir(), "bom.yaml")
	if err := os.WriteFile(p, append([]byte{0xEF, 0xBB, 0xBF}, body...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("UTF-8 BOM должен переживаться: %v", err)
	}
}

// TestErrors — валидация даёт понятные ошибки с именем поля.
func TestErrors(t *testing.T) {
	in := t.TempDir() // YAML в одинарных кавычках: обратные слэши литеральны
	base := "inputs:\n  - '" + in + "'\nsink: 'clickhouse://l:9001/tj'\nstate_dir: s\n"
	cases := []struct {
		name, yaml, wantSub string
	}{
		{"нет inputs", "sink: 'clickhouse://l:9001/tj'\nstate_dir: s\n", "inputs"},
		{"несуществующий каталог", "inputs:\n  - 'C:\\nonexistent_tj_dir_42'\nsink: 'clickhouse://l:9001/tj'\nstate_dir: s\n", "inputs[0]"},
		{"нет sink", "inputs:\n  - '" + in + "'\nstate_dir: s\n", "sink"},
		{"не clickhouse", "inputs:\n  - '" + in + "'\nsink: 'postgres://x'\nstate_dir: s\n", "sink"},
		{"нет state_dir", "inputs:\n  - '" + in + "'\nsink: 'clickhouse://l:9001/tj'\n", "state_dir"},
		{"poll_ms мал", base + "poll_ms: 5\n", "poll_ms"},
		{"poll_ms велик", base + "poll_ms: 999999\n", "poll_ms"},
		{"idle_close_ms", base + "idle_close_ms: 5\n", "idle_close_ms"},
		{"flush_ms", base + "flush_ms: 0\n", "flush_ms"},
		{"batch_rows", base + "batch_rows: 0\n", "batch_rows"},
		{"batch_bytes", base + "batch_bytes: 0\n", "batch_bytes"},
		{"threads", base + "threads: 0\n", "threads"},
		{"metrics без порта", base + "metrics: 'localhost'\n", "metrics"},
		{"log_level", base + "log_level: verbose\n", "log_level"},
		{"незнакомый ключ", base + "polll_ms: 100\n", "polll_ms"},
		{"кривой YAML", "inputs: [\n", "конфиг"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.yaml))
			if err == nil {
				t.Fatalf("ожидалась ошибка с %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("в ошибке %q нет %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "нет.yaml"))
	if err == nil || !strings.Contains(err.Error(), "конфиг") {
		t.Fatalf("ожидалась ошибка чтения файла, есть %v", err)
	}
}
