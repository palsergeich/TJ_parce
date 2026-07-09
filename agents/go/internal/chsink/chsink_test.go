package chsink

import (
	"math"
	"testing"
	"time"

	"tjagent/internal/parser"
)

func TestEventTime(t *testing.T) {
	got := EventTime("2025-11-30T16:", []byte("06:58.904004"))
	want := time.Date(2025, 11, 30, 16, 6, 58, 904004000, time.UTC)
	if !got.Equal(want) {
		t.Errorf("EventTime = %v, want %v", got, want)
	}
	// Деградированный timestamp (пустой префикс) → нулевое время
	if got := EventTime("", []byte("00:53.520012")); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("деградированный timestamp: %v, want 1970-01-01", got)
	}
	// «Месяц 13» не валидируется — time.Date нормализует переносом (спека §3)
	if got := EventTime("2025-13-30T21:", []byte("00:00.000000")); got.Year() != 2026 || got.Month() != time.January {
		t.Errorf("месяц 13: %v, ожидался перенос в 2026-01", got)
	}
	// Некондиционный вход → нулевое время, без паники
	if got := EventTime("20xx-11-30T16:", []byte("06:58.904004")); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("мусорный префикс: %v, want 1970-01-01", got)
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]uint64{
		"0":                    0,
		"000":                  0,
		"007":                  7,
		"17500000000":          17500000000,
		"18446744073709551615": math.MaxUint64,
		"18446744073709551616": math.MaxUint64, // переполнение — насыщение
		"99999999999999999999": math.MaxUint64,
	}
	for in, want := range cases {
		if got := ParseDuration([]byte(in)); got != want {
			t.Errorf("ParseDuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestRowBuilderBuild(t *testing.T) {
	b := NewRowBuilder(false, false)
	ev := []byte("06:58.904004-1500,CONN,2,process=rphost,OSThread=4188,Txt='a''b',Dup=1,Dup=2\r\n")
	f, ok := parser.ParseEventFields(ev)
	if !ok {
		t.Fatal("событие отброшено")
	}
	r := b.Build(f, "2025-11-30T16:", "25113016.log", `Diag_86\rphost_1\25113016.log`)

	if !r.Time.Equal(time.Date(2025, 11, 30, 16, 6, 58, 904004000, time.UTC)) {
		t.Errorf("Time = %v", r.Time)
	}
	if r.Duration != 1500 || r.Event != "CONN" || r.Level != "2" {
		t.Errorf("заголовок: %+v", r)
	}
	if r.Filename != "25113016.log" || r.FilePath != `Diag_86\rphost_1\25113016.log` {
		t.Errorf("файл: %+v", r)
	}
	want := Props{
		{"process", "rphost"},
		{"OSThread", "4188"},
		{"Txt", "a'b"},
		{"Dup", "2"}, // последнее значение побеждает, позиция первая
	}
	if len(r.Props) != len(want) {
		t.Fatalf("props = %+v, want %+v", r.Props, want)
	}
	for i := range want {
		if r.Props[i] != want[i] {
			t.Errorf("props[%d] = %+v, want %+v", i, r.Props[i], want[i])
		}
	}
	if r.bytes <= 0 {
		t.Error("оценка байтов строки не заполнена")
	}

	// Числовой уровень — десятичный текст; строковый — как есть
	f2, _ := parser.ParseEventFields([]byte("00:00.000001-0,EXCP,Pad=xxx"))
	r2 := b.Build(f2, "", "x.log", "x")
	if r2.Level != "Pad=xxx" || len(r2.Props) != 0 {
		t.Errorf("short_header: level=%q props=%+v", r2.Level, r2.Props)
	}
	if !r2.Time.Equal(time.Unix(0, 0)) {
		t.Errorf("деградированный timestamp: %v", r2.Time)
	}

	// Интернирование: одинаковые имена → одна строка (общий backing)
	f3, _ := parser.ParseEventFields([]byte("06:58.904004-1,CONN,2,process=x"))
	r3 := b.Build(f3, "2025-11-30T16:", "a.log", "a")
	if r3.Event != "CONN" || r3.Props[0].Name != "process" {
		t.Errorf("интернирование сломано: %+v", r3)
	}
}

func TestParseSinkDSN(t *testing.T) {
	cases := []struct{ dsn, wantDSN, wantTable, wantSchema string }{
		{"clickhouse://localhost:9001/tj_bench", "clickhouse://localhost:9001/tj_bench", "events", "bench"},
		{"clickhouse://localhost:9001/tj_bench?table=events_go", "clickhouse://localhost:9001/tj_bench", "events_go", "bench"},
		{"clickhouse://u:p@h:9001/db?dial_timeout=2s&table=t1", "clickhouse://u:p@h:9001/db?dial_timeout=2s", "t1", "bench"},
		{"clickhouse://localhost:9001/tj?schema=rich", "clickhouse://localhost:9001/tj", "events", "rich"},
		{"clickhouse://localhost:9001/tj?schema=rich&table=events", "clickhouse://localhost:9001/tj", "events", "rich"},
		{"clickhouse://localhost:9001/tj?schema=bench", "clickhouse://localhost:9001/tj", "events", "bench"},
	}
	for _, c := range cases {
		dsn, table, schema, err := parseSinkDSN(c.dsn)
		if err != nil {
			t.Errorf("parseSinkDSN(%q): %v", c.dsn, err)
			continue
		}
		if dsn != c.wantDSN || table != c.wantTable || schema != c.wantSchema {
			t.Errorf("parseSinkDSN(%q) = (%q, %q, %q), want (%q, %q, %q)",
				c.dsn, dsn, table, schema, c.wantDSN, c.wantTable, c.wantSchema)
		}
	}
	if _, _, _, err := parseSinkDSN("clickhouse://localhost:9001/tj?schema=wide"); err == nil {
		t.Error("неизвестная схема обязана давать ошибку")
	}
	if !tableRe.MatchString("tj_bench.events_go") || tableRe.MatchString("bad-name") || tableRe.MatchString("a.b.c") {
		t.Error("tableRe: неверная валидация")
	}
}
