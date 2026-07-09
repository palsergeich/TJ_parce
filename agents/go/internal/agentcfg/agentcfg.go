// Package agentcfg — файл конфигурации агента (--config <path>, YAML).
// Формат выбран YAML (gopkg.in/yaml.v3, запиннен в go.mod): человекочитаемые
// комментарии в примере, минимальная зависимость. Эталонный пример —
// agents/go/tj-agent.example.yaml.
//
// Правила слияния: значения файла — база, явные CLI-флаги перекрывают
// (сборка эффективной конфигурации — в cmd/tj-agent-go). Незнакомые ключи
// файла — ошибка (KnownFields: опечатка в имени параметра не должна молча
// превращаться в значение по умолчанию).
package agentcfg

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config — содержимое файла конфигурации. Отсутствующие ключи получают
// значения Default() (decode поверх заполненной структуры).
type Config struct {
	// Inputs — отслеживаемые каталоги ТЖ (рекурсивно, *.log). Обязателен ≥1.
	Inputs []string `yaml:"inputs"`
	// Sink — DSN ClickHouse: clickhouse://host:port/db[?schema=rich&table=...].
	// Продуктовый онлайн-режим — schema=rich в tj.events.
	Sink string `yaml:"sink"`
	// StateDir — каталог чекпоинтов (checkpoints.json). Обязателен.
	StateDir string `yaml:"state_dir"`
	// StopFile — путь stop-файла. Опционален: службу останавливает сигнал SCM,
	// консольный запуск — Ctrl+C; stop-файл остаётся третьим способом.
	StopFile string `yaml:"stop_file"`

	PollMS      int   `yaml:"poll_ms"`
	IdleCloseMS int   `yaml:"idle_close_ms"`
	FlushMS     int   `yaml:"flush_ms"`
	BatchRows   int   `yaml:"batch_rows"`
	BatchBytes  int64 `yaml:"batch_bytes"`
	Threads     int   `yaml:"threads"`

	// SQLNorm — нормализация SQL rich-схемы (sql_norm_hash / param_count /
	// sql_params, docs/sql-normalization.md). По умолчанию включена; false
	// отключает вычисление и убирает колонки из INSERT (совместимость с
	// tj.events без миграции 002_sql_norm.sql).
	SQLNorm bool `yaml:"sql_norm"`

	// ContextSKDSmart — умная значимая строка контекста для СКД
	// (docs/context-line.md): если последняя строка стека — вызов вывода
	// результата компоновки (Вывести/ВывестиЭлемент/НачатьВывод/
	// Инициализировать компоновки), context_line берётся из строки модуля
	// Отчет.*/Обработка.* выше по стеку. context/context_hash не меняются.
	// По умолчанию включена; false — всегда последняя непустая строка.
	ContextSKDSmart bool `yaml:"context_skd_smart"`

	// Metrics — адрес /metrics (host:port или :port); пусто — endpoint выключен.
	Metrics string `yaml:"metrics"`
	// LogLevel — error | info | debug.
	LogLevel string `yaml:"log_level"`
	// LogFile — файл журнала агента (append); пусто — stderr. Служба Windows
	// при пустом значении пишет в <state_dir>\tj-agent-go.log (stderr службы
	// в никуда).
	LogFile string `yaml:"log_file"`
	// StatsJSON — путь итоговой статистики (контракт bakeoff §3); опционален.
	StatsJSON string `yaml:"stats_json"`
}

// Значения по умолчанию (совпадают с CLI-умолчаниями follow-режима).
const (
	DefaultPollMS      = 500
	DefaultIdleCloseMS = 2000
	DefaultFlushMS     = 1000
	DefaultBatchRows   = 50000
	DefaultBatchBytes  = 64 << 20
)

// Default — конфигурация со значениями по умолчанию (без обязательных полей).
func Default() Config {
	threads := runtime.NumCPU()
	if threads < 1 {
		threads = 1
	}
	if threads > 1024 {
		threads = 1024
	}
	return Config{
		PollMS:          DefaultPollMS,
		IdleCloseMS:     DefaultIdleCloseMS,
		FlushMS:         DefaultFlushMS,
		BatchRows:       DefaultBatchRows,
		BatchBytes:      DefaultBatchBytes,
		Threads:         threads,
		SQLNorm:         true,
		ContextSKDSmart: true,
		LogLevel:        "info",
	}
}

// Load читает и валидирует файл конфигурации. Ошибки — с путём файла и
// именем поля: «конфиг C:\...\a.yaml: поле 'poll_ms': ...».
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("конфиг %s: %w", path, err)
	}
	// UTF-8 BOM у файлов, сохранённых Windows-редакторами, — норма; yaml его не ест.
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})

	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("конфиг %s: %v", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("конфиг %s: %w", path, err)
	}
	return cfg, nil
}

// Validate проверяет заполненность обязательных полей, диапазоны (те же, что
// у CLI-флагов) и существование каталогов inputs.
func (c *Config) Validate() error {
	if len(c.Inputs) == 0 {
		return fmt.Errorf("поле 'inputs': нужен хотя бы один каталог ТЖ")
	}
	for i, in := range c.Inputs {
		if strings.TrimSpace(in) == "" {
			return fmt.Errorf("поле 'inputs[%d]': пустой путь", i)
		}
		st, err := os.Stat(in)
		if err != nil {
			return fmt.Errorf("поле 'inputs[%d]': каталог не существует: %s", i, in)
		}
		if !st.IsDir() {
			return fmt.Errorf("поле 'inputs[%d]': не каталог: %s", i, in)
		}
	}
	if strings.TrimSpace(c.Sink) == "" {
		return fmt.Errorf("поле 'sink': обязателен DSN ClickHouse (clickhouse://host:port/db[?schema=rich])")
	}
	if !strings.HasPrefix(c.Sink, "clickhouse") {
		return fmt.Errorf("поле 'sink': follow-режим поддерживает только ClickHouse, ожидается clickhouse://... (получено %q)", c.Sink)
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return fmt.Errorf("поле 'state_dir': обязателен каталог чекпоинтов")
	}
	if c.PollMS < 10 || c.PollMS > 60_000 {
		return fmt.Errorf("поле 'poll_ms': %d вне диапазона 10..60000", c.PollMS)
	}
	if c.IdleCloseMS < 100 || c.IdleCloseMS > 600_000 {
		return fmt.Errorf("поле 'idle_close_ms': %d вне диапазона 100..600000", c.IdleCloseMS)
	}
	if c.FlushMS < 1 || c.FlushMS > 3_600_000 {
		return fmt.Errorf("поле 'flush_ms': %d вне диапазона 1..3600000", c.FlushMS)
	}
	if c.BatchRows < 1 || c.BatchRows > 10_000_000 {
		return fmt.Errorf("поле 'batch_rows': %d вне диапазона 1..10000000", c.BatchRows)
	}
	if c.BatchBytes < 1 || c.BatchBytes > 1<<40 {
		return fmt.Errorf("поле 'batch_bytes': %d вне диапазона 1..2^40", c.BatchBytes)
	}
	if c.Threads < 1 || c.Threads > 1024 {
		return fmt.Errorf("поле 'threads': %d вне диапазона 1..1024", c.Threads)
	}
	if c.Metrics != "" {
		if _, _, err := net.SplitHostPort(c.Metrics); err != nil {
			return fmt.Errorf("поле 'metrics': ожидается host:port или :port (получено %q)", c.Metrics)
		}
	}
	switch c.LogLevel {
	case "error", "info", "debug":
	default:
		return fmt.Errorf("поле 'log_level': %q (допустимо error | info | debug)", c.LogLevel)
	}
	return nil
}
