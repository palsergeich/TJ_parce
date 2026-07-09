package chsink

import (
	"strings"
	"testing"
	"time"

	"tjagent/internal/parser"

	"github.com/go-faster/city"
)

// Значения-эталоны сняты с живого ClickHouse 24.8 (tj-clickhouse) батареями
// запросов — см. комментарии chnum.go/rich.go.

func TestChUintOrZero(t *testing.T) {
	u32 := map[string]uint32{
		"":            0,
		"0":           0,
		"00":          0,
		"007":         7,
		"+5":          5, // ведущий '+' допустим
		"-1":          0,
		" 5":          0, // пробелы не допускаются
		"5 ":          0,
		"5abc":        0,
		"1.5":         0,
		"1e3":         0,
		"4294967295":  4294967295,
		"4294967296":  0, // переполнение → 0, не насыщение
		"586(581)":    0,
		"abc":         0,
		"+":           0,
		"10000000000": 0,
	}
	for in, want := range u32 {
		if got := chUint32OrZero(in); got != want {
			t.Errorf("chUint32OrZero(%q) = %d, want %d", in, got, want)
		}
	}
	u64 := map[string]uint64{
		"":                     0,
		"007":                  7,
		"18446744073709551615": 18446744073709551615,
		"18446744073709551616": 0,
		"1e3":                  0,
		"1.5":                  0,
		"+7":                   7,
		"-0":                   0,
	}
	for in, want := range u64 {
		if got := chUint64OrZero(in); got != want {
			t.Errorf("chUint64OrZero(%q) = %d, want %d", in, got, want)
		}
	}
	u8 := map[string]uint8{"255": 255, "256": 0, "-1": 0, "01": 1, "2": 2, "+2": 2}
	for in, want := range u8 {
		if got := chUint8OrZero(in); got != want {
			t.Errorf("chUint8OrZero(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChInt64OrZero(t *testing.T) {
	cases := map[string]int64{
		"-123":                 -123,
		"-9223372036854775808": -9223372036854775808,
		"-9223372036854775809": 0,
		"9223372036854775807":  9223372036854775807,
		"9223372036854775808":  0,
		"+42":                  42,
		"-0":                   0,
		"007":                  7,
		"1.5":                  0,
		"":                     0,
		"-":                    0,
		"+":                    0,
	}
	for in, want := range cases {
		if got := chInt64OrZero(in); got != want {
			t.Errorf("chInt64OrZero(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseWaitConns(t *testing.T) {
	cases := []struct {
		in   string
		want []uint32
	}{
		{"", nil},
		{"5", []uint32{5}},
		{"7,9,11", []uint32{7, 9, 11}},
		{"7, 9 ,11,", []uint32{7, 9, 11, 0}}, // хвостовая пустота → 0 (снято с сервера)
		{"\t5", []uint32{0}},                 // trimBoth НЕ срезает табуляцию
		{" 5 , 6", []uint32{5, 6}},
		{"a,1", []uint32{0, 1}},
	}
	for _, c := range cases {
		got := parseWaitConns(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseWaitConns(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseWaitConns(%q)[%d] = %d, want %d", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"a":                  "a",
		"a\nb":               "b",
		"a\nb\n":             "b",
		"a\nb\n\n":           "b",
		"\r\n\r\n":           "",
		"abc\rdef":           "abcdef", // \r внутри строки выбрасывается (replaceAll)
		"x\n \n":             " ",      // строка из пробела непуста (импортёр не трогает пробелы)
		"a\r\nb\r":           "b",
		"a\n\rb":             "b",
		"\tконец\t\r\n":      "\tконец\t", // табы сохраняются
		"первая\nвторая\r\n": "вторая",
	}
	for in, want := range cases {
		if got := lastNonEmptyLine(in); got != want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstPathSegment(t *testing.T) {
	cases := map[string]string{
		`Diag_86\rphost_1\f.log`: "Diag_86",
		`Diag_86/rphost_1/f.log`: "Diag_86",
		"noSep":                  "noSep",
		"":                       "",
		`\lead`:                  "",
	}
	for in, want := range cases {
		if got := firstPathSegment(in); got != want {
			t.Errorf("firstPathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCityHash64Compat — закреплённые значения cityHash64 живого ClickHouse
// (батарея 25 строк: ASCII, кириллица, \r\n, границы длины 16/17/32/33/63/64/65/128/1000).
// go-faster/city.CH64 обязан совпадать байт-в-байт — контракт непрерывности
// с 121.5 млн уже импортированных строк tj.events.
func TestCityHash64Compat(t *testing.T) {
	cases := []struct {
		s    string
		want uint64
	}{
		{"", 11160318154034397263},
		{"a", 2603192927274642682},
		{"abc", 4220206313085259313},
		{"0", 10408321403207385874},
		{" ", 14535776232712007899},
		{"\t", 12478783090956271283},
		{"\r\n", 11389288376112400146},
		{"0123456789abcdef", 692747204941329175},
		{"0123456789abcdefg", 792568009951096890},
		{"0123456789abcdef0123456789abcdef", 1759797222395115254},
		{"0123456789abcdef0123456789abcdefX", 1479925382395186429},
		{strings.Repeat("x", 63), 14780008506563289803},
		{strings.Repeat("x", 64), 6437053381938498259},
		{strings.Repeat("x", 65), 5260653789997849295},
		{strings.Repeat("x", 128), 1142585757146322653},
		{strings.Repeat("y", 1000), 12790817935733939643},
		{"Форма.Вызов : ОбщийМодуль.ПроверкаПрав.Модуль : 15", 14938437809881414002},
		{"й", 5594004165929152478},
		{"Привет, мир! \U0001F30D ẞ ß", 2050678182429446975},
		{"line1\nline2", 10191124054214940562},
		{"line1\r\nline2\r\n", 17653207398966794104},
		{"\tСеанс : Форма.Вызов\r\n\tОбщийМодуль.РаботаСФайлами : 120\r\n\tпоследняя строка\t", 2396800502279957494},
		{"SELECT T1._Fld123 FROM _InfoRg456 T1 WHERE T1._Period >= ?", 2786699645741999914},
		{"\x01\x02\x03\x1f\x7f", 8270194078738738223},
		{"конец", 5232072778368997737},
	}
	for _, c := range cases {
		if got := city.CH64([]byte(c.s)); got != c.want {
			t.Errorf("city.CH64(%.30q) = %d, want %d (cityHash64 ClickHouse)", c.s, got, c.want)
		}
	}
}

func TestRichEventTime(t *testing.T) {
	valid := richEventTime("2025-11-30T16:", []byte("06:58.904004"))
	want := time.Date(2025, 11, 30, 16, 6, 58, 904004000, time.UTC)
	if !valid.Equal(want) {
		t.Errorf("валидная дата: %v, want %v", valid, want)
	}
	// Сверка эпохи с сервером: toUnixTimestamp64Micro(...) = 1764518818904004
	if got := valid.UnixMicro(); got != 1764518818904004 {
		t.Errorf("UnixMicro = %d, want 1764518818904004 (значение живого сервера)", got)
	}
	epoch := time.Unix(0, 0).UTC()
	degenerate := []struct {
		prefix string
		tp     string
	}{
		{"", "06:58.904004"},               // деградированный timestamp
		{"2025-13-30T21:", "00:00.000000"}, // месяц 13 → NULL у parseDateTime64BestEffortOrNull
		{"2025-02-30T10:", "00:00.000000"}, // 30 февраля
		{"2025-00-10T10:", "00:00.000000"}, // месяц 00
		{"2025-11-00T10:", "00:00.000000"}, // день 00
		{"2025-11-30T24:", "00:00.000000"}, // час 24
		{"2025-11-30T16:", "60:00.000000"}, // минута 60 (маска события это допускает)
		{"2025-11-30T16:", "00:61.000000"}, // секунда 61
	}
	for _, d := range degenerate {
		if got := richEventTime(d.prefix, []byte(d.tp)); !got.Equal(epoch) {
			t.Errorf("richEventTime(%q, %q) = %v, want эпоха", d.prefix, d.tp, got)
		}
	}
	// Високосный февраль валиден
	if got := richEventTime("2024-02-29T00:", []byte("00:00.000000")); got.Equal(epoch) {
		t.Error("2024-02-29 валидна (високосный год)")
	}
	// В отличие от bench-нормализации (EventTime переносит месяц 13 в январь)
	if benchGot := EventTime("2025-13-30T21:", []byte("00:00.000000")); benchGot.Equal(epoch) {
		t.Error("прекондиция: bench EventTime нормализует месяц 13, а не деградирует")
	}
}

// TestBuildRich — полный маппинг события в продуктовую схему: семантика
// импортёра (см. rich.go). Кейс покрывает: горячие колонки (в т.ч. имена
// с двоеточием), первое-вхождение при дубликатах, приоритеты Sql|Query|Sdbl и
// Descr|Txt|txt, фолбэк t:clientID→ClientID, правило SessionID, WaitConnections,
// контекст (хэш+последняя строка), хвост props с дубликатами, src_line=0.
func TestBuildRich(t *testing.T) {
	b := NewRowBuilder(true, true)
	ev := []byte("06:58.904004-1500,DBMSSQL,2," +
		"process=rphost,p:processName=srv,OSThread=4188," +
		"t:clientID=,ClientID=77,t:connectID=9," +
		"SessionID=586(581),SessionID=5," +
		"Usr=Иванов,t:applicationName=1CV8C,t:computerName=PC-01,AppID=app," +
		"DBMS=DBMSSQL,DataBase=srv\\base,dbpid=1234,Trans=1,Rows=10,RowsAffected=2," +
		"CpuTime=15600,Memory=-512,MemoryPeak=8192,InBytes=100,OutBytes=200,callWait=5," +
		"IName=IFace,MName=Meth,Func=F1,Module=Mod," +
		"Context='строка1\r\nстрока2\t',Query='SELECT 1',Sdbl='SDBL text'," +
		"planSQLText='plan',Txt='описание',Exception=Exc," +
		"Regions='InfoRg.Lock',WaitConnections='7, 9 ,11',Locks='dump',DeadlockConnectionIntersections='dg'," +
		"first=extra,first=extra2,timestamp=meta,Дубль=1\r\n")
	f, ok := parser.ParseEventFields(ev)
	if !ok {
		t.Fatal("событие отброшено")
	}
	r := b.Build(f, "2025-11-30T16:", "25113016.log", `Diag_86\rphost_2408\25113016.log`)
	x := r.Rich
	if x == nil {
		t.Fatal("RichExt не построен")
	}

	if !x.Time.Equal(time.Date(2025, 11, 30, 16, 6, 58, 904004000, time.UTC)) {
		t.Errorf("Time = %v", x.Time)
	}
	if x.DurationUs != 1500 || r.Event != "DBMSSQL" || r.Level != "2" {
		t.Errorf("заголовок: dur=%d event=%q level=%q", x.DurationUs, r.Event, r.Level)
	}
	if x.Collection != "Diag_86" {
		t.Errorf("Collection = %q", x.Collection)
	}
	if x.Process != "rphost" || x.ProcessName != "srv" || x.OSThread != 4188 {
		t.Errorf("process: %+v", x)
	}
	// t:clientID пуст → фолбэк ClientID
	if x.ClientID != 77 || x.ConnectID != 9 {
		t.Errorf("client/connect: %d/%d", x.ClientID, x.ConnectID)
	}
	// SessionID: первое значение '586(581)' → 0; вторая пара '5' разобралась → в props не идёт
	if x.SessionID != 0 {
		t.Errorf("SessionID = %d, want 0", x.SessionID)
	}
	if x.Usr != "Иванов" || x.AppName != "1CV8C" || x.ComputerName != "PC-01" || x.AppID != "app" {
		t.Errorf("сеанс: %+v", x)
	}
	if x.DBMS != "DBMSSQL" || x.DBName != `srv\base` || x.DBPid != 1234 || x.Trans != 1 {
		t.Errorf("СУБД: %+v", x)
	}
	if x.RowsRet != 10 || x.RowsAffected != 2 || x.CPUTimeUs != 15600 ||
		x.Memory != -512 || x.MemoryPeak != 8192 || x.InBytes != 100 || x.OutBytes != 200 || x.CallWaitUs != 5 {
		t.Errorf("ресурсы: %+v", x)
	}
	if x.IfaceName != "IFace" || x.MethodName != "Meth" || x.FuncName != "F1" || x.Module != "Mod" {
		t.Errorf("интерфейс: %+v", x)
	}
	wantCtx := "строка1\r\nстрока2\t"
	if x.Context != wantCtx {
		t.Errorf("Context = %q", x.Context)
	}
	if x.ContextHash != city.CH64([]byte(wantCtx)) || x.ContextHash == 0 {
		t.Errorf("ContextHash = %d", x.ContextHash)
	}
	if x.ContextLine != "строка2\t" {
		t.Errorf("ContextLine = %q", x.ContextLine)
	}
	// Sql пуст → Query (Sdbl игнорируется)
	if x.SQLText != "SELECT 1" || x.SQLHash != city.CH64([]byte("SELECT 1")) {
		t.Errorf("SQL: %q / %d", x.SQLText, x.SQLHash)
	}
	// Нормализация: 'SELECT 1' → 'SELECT ?', param_count=1, params=['1']
	if x.SQLNormHash != city.CH64([]byte("SELECT ?")) {
		t.Errorf("SQLNormHash = %d, want cityHash64('SELECT ?')", x.SQLNormHash)
	}
	if x.ParamCount != 1 || len(x.SQLParams) != 1 || x.SQLParams[0] != "1" {
		t.Errorf("нормализация: count=%d params=%q", x.ParamCount, x.SQLParams)
	}
	if x.PlanText != "plan" {
		t.Errorf("PlanText = %q", x.PlanText)
	}
	// Descr пуст → Txt
	if x.Descr != "описание" || x.Exception != "Exc" {
		t.Errorf("descr/exception: %q/%q", x.Descr, x.Exception)
	}
	if x.LockRegions != "InfoRg.Lock" || x.LocksDump != "dump" || x.DeadlockGraph != "dg" {
		t.Errorf("блокировки: %+v", x)
	}
	if len(x.LockWaitConns) != 3 || x.LockWaitConns[0] != 7 || x.LockWaitConns[1] != 9 || x.LockWaitConns[2] != 11 {
		t.Errorf("LockWaitConns = %v", x.LockWaitConns)
	}

	// Хвост props: SessionID='586(581)' (не разобрался), first ×2 (дубликаты
	// сохраняются), Дубль=1. NDJSON-мета 'timestamp=meta' исключена. Query/Sdbl/
	// Txt и прочие горячие — исключены.
	want := Props{
		{"SessionID", "586(581)"},
		{"first", "extra"},
		{"first", "extra2"},
		{"Дубль", "1"},
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

	// Второе событие тем же builder'ом: скретч richHot обязан быть чистым
	f2, _ := parser.ParseEventFields([]byte("00:01.000000-5,CALL,1,Context='x'"))
	r2 := b.Build(f2, "2025-11-30T16:", "a.log", "a")
	if r2.Rich.Context != "x" || r2.Rich.SQLText != "" || r2.Rich.Usr != "" || len(r2.Props) != 0 {
		t.Errorf("скретч builder'а грязный: %+v props=%+v", r2.Rich, r2.Props)
	}
	// Пустой контекст → нулевые хэши (нормализация не считается без sql_text)
	f3, _ := parser.ParseEventFields([]byte("00:01.000000-5,CALL,1,Usr=U"))
	r3 := b.Build(f3, "2025-11-30T16:", "a.log", "a")
	if r3.Rich.ContextHash != 0 || r3.Rich.SQLHash != 0 || r3.Rich.ContextLine != "" {
		t.Errorf("пустые тексты: %+v", r3.Rich)
	}
	if r3.Rich.SQLNormHash != 0 || r3.Rich.ParamCount != 0 || r3.Rich.SQLParams != nil {
		t.Errorf("нормализация пустого sql_text: %+v", r3.Rich)
	}
	// Sql имеет приоритет над Query
	f4, _ := parser.ParseEventFields([]byte("00:01.000000-5,SDBL,1,Sdbl='S3',Sql='S1',Query='S2'"))
	r4 := b.Build(f4, "2025-11-30T16:", "a.log", "a")
	if r4.Rich.SQLText != "S1" {
		t.Errorf("приоритет Sql: %q", r4.Rich.SQLText)
	}
	// t:clientID непустой — ClientID игнорируется; SessionID='0' и '' в props не идут
	f5, _ := parser.ParseEventFields([]byte("00:01.000000-5,CALL,1,t:clientID=8,ClientID=77,SessionID=0,SessionID="))
	r5 := b.Build(f5, "2025-11-30T16:", "a.log", "a")
	if r5.Rich.ClientID != 8 || r5.Rich.SessionID != 0 || len(r5.Props) != 0 {
		t.Errorf("t:clientID/SessionID: %+v props=%+v", r5.Rich, r5.Props)
	}
}

// TestBuildRichSQLNorm — маппинг нормализации: доминирующий шаблон корпуса
// (тело с '?' + хвост p_N) и выключатель sql_norm.
func TestBuildRichSQLNorm(t *testing.T) {
	// Свойство Sql с хвостом p_N: значения хвоста — в params, хвост вырезан
	// из нормы. В сыром событии перевод строки внутри значения — реальные
	// \r\n (parser отдаёт значение после расклейки кавычек).
	b := NewRowBuilder(true, true)
	ev := []byte("00:01.000000-5,DBMSSQL,1,Sql='SELECT x FROM t WHERE a = ? AND b IN (?, ?)\r\n" +
		"p_0: 0x01\r\np_1: 5\r\np_2: 7\r\n'")
	f, ok := parser.ParseEventFields(ev)
	if !ok {
		t.Fatal("событие отброшено")
	}
	r := b.Build(f, "2025-11-30T16:", "a.log", "a")
	x := r.Rich
	wantNorm := "SELECT x FROM t WHERE a = ? AND b IN (?)"
	if x.SQLNormHash != city.CH64([]byte(wantNorm)) {
		t.Errorf("SQLNormHash не совпал с cityHash64(%q)", wantNorm)
	}
	if x.ParamCount != 3 || len(x.SQLParams) != 3 {
		t.Fatalf("count=%d params=%q", x.ParamCount, x.SQLParams)
	}
	for i, want := range []string{"0x01", "5", "7"} {
		if x.SQLParams[i] != want {
			t.Errorf("params[%d] = %q, want %q", i, x.SQLParams[i], want)
		}
	}
	// sql_hash сырого текста не зависит от нормализации
	if x.SQLHash == 0 || x.SQLHash == x.SQLNormHash {
		t.Errorf("SQLHash = %d, SQLNormHash = %d", x.SQLHash, x.SQLNormHash)
	}

	// Выключатель: rich без sql_norm → нулевые поля нормализации
	boff := NewRowBuilder(true, false)
	f2, _ := parser.ParseEventFields([]byte("00:01.000000-5,DBMSSQL,1,Sql='SELECT 1'"))
	r2 := boff.Build(f2, "2025-11-30T16:", "a.log", "a")
	if r2.Rich.SQLNormHash != 0 || r2.Rich.ParamCount != 0 || r2.Rich.SQLParams != nil {
		t.Errorf("sql_norm=false: %+v", r2.Rich)
	}
	if r2.Rich.SQLHash == 0 {
		t.Error("sql_hash обязан считаться и при выключенной нормализации")
	}
}

// TestBuildRichDurationNoSaturation — rich-режим повторяет toUInt64OrZero:
// переполнение uint64 даёт 0 (bench-путь насыщает до MaxUint64).
func TestBuildRichDurationNoSaturation(t *testing.T) {
	b := NewRowBuilder(true, true)
	f, _ := parser.ParseEventFields([]byte("00:01.000000-18446744073709551616,CALL,1,Usr=U"))
	r := b.Build(f, "2025-11-30T16:", "a.log", "a")
	if r.Rich.DurationUs != 0 {
		t.Errorf("rich duration = %d, want 0 (toUInt64OrZero)", r.Rich.DurationUs)
	}
	if r.Duration == 0 {
		t.Error("bench-поле Duration обязано насыщаться, а не обнуляться")
	}
}
