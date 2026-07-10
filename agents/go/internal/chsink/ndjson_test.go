// ndjson_test.go — дифференциальная приёмка реплея из дискового буфера:
// строка, восстановленная BuildNDJSON из NDJSON-вывода parser.AppendEvent,
// обязана быть НЕОТЛИЧИМОЙ от строки прямого пути ParseEventFields → Build —
// включая порядок/дубликаты props, все поля RichExt (sqlnorm, context_line)
// и внутренний учёт bytes (порог батча). Расхождение = разъезд данных между
// прямой вставкой и вставкой после простоя ClickHouse.
package chsink

import (
	"reflect"
	"strings"
	"testing"

	"tjagent/internal/parser"
)

// mirrorEvents — события-ловушки: маска+заголовок валидны (иначе прямой путь
// даёт parse_skip и в буфер событие не попадает).
var mirrorEvents = []struct {
	name       string
	datePrefix string
	filename   string
	filePath   string
	ev         string
}{
	{"минимальное", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"00:01.000001-0,CALL,1\r\n"},
	{"обычное_с_props", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-12345,CALL,3,Usr=Иванов,Memory=42,GenMs=1764331000123\r\n"},
	{"канонизация_duration", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-000123,CALL,1,Usr=x\r\n"},
	{"duration_нули", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-000,CALL,1,Usr=x\r\n"},
	{"level_строка", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,EXCP,W,Descr='ошибка'\r\n"},
	{"level_съедает_остаток", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,вся оставшаяся часть события без запятой\r\n"},
	{"дубликаты_ключей", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Usr=первый,Usr=второй,Memory=1,Memory=2\r\n"},
	{"кавычки_апострофы", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Descr='значение с '' кавычкой',Next=1\r\n"},
	{"кавычки_двойные", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Descr=\"а \"\"б\"\" в\",Next=2\r\n"},
	{"многострочный_context", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Context='ОбщийМодуль.Тест : 5 : строка1\r\nФорма.Обработчик : 7 : строка2',Usr=y\r\n"},
	{"context_скд_хвост", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Context='Отчет.Продажи.МодульОбъекта : 10 : СкомпоноватьРезультат\r\nОбщийМодуль.КомпоновкаДанных.Модуль : 100 : ПроцессорВывода.Вывести(Документ)'\r\n"},
	{"sql_с_литералами", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,DBMSSQL,2,Sql=\"SELECT * FROM T WHERE a = 'абв' AND b = 42 AND c = 3.14\",Rows=7\r\n"},
	{"sql_fallback_query", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,DBMSSQL,2,Query='SELECT 1 WHERE x=''y''',RowsAffected=3\r\n"},
	{"sessionid_число_и_мусор", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,SessionID=123,SessionID=abc,SessionID=0,SessionID=\r\n"},
	{"clientid_фолбэк", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,ClientID=17,OSThread=99,t:connectID=5\r\n"},
	{"t_clientid_приоритет", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,t:clientID=8,ClientID=17\r\n"},
	{"числовая_типизация", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,A=42,B=-3.5,C=1e10,D=007,E=1.,SearchString=123,Guid=42\r\n"},
	{"пустые_значения", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,A=,B='',C=\"\",D=x,E=\r\n"},
	{"деградированный_файл", "", "not_a_date.log", `coll\rphost_1\not_a_date.log`,
		"12:34.567890-1,CALL,1,Usr=x\r\n"},
	{"невалидная_дата_месяц13", "2025-13-28T14:", "25132814.log", `coll\rphost_1\25132814.log`,
		"12:34.567890-1,CALL,1,Usr=x\r\n"},
	{"экранирование_в_имени", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Имя\"со\\спец=значение\tс\tтабами,N=1\r\n"},
	{"невалидный_utf8_KI3", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,Bin='а\xffб\xfe',N=2\r\n"},
	{"waitconnections", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,TLOCK,1,Regions=R1.DIMS,WaitConnections=7, 9 ,,Locks='a b'\r\n"},
	{"гигантские_числа", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-99999999999999999999999,CALL,1,Memory=-9223372036854775808,CpuTime=18446744073709551615\r\n"},
	{"deadlock_поле", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,TDEADLOCK,1,DeadlockConnectionIntersections='1 2 3'\r\n"},
	{"хвост_без_равно", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,A=1,мусор без знака равно\r\n"},
	{"имя_как_мета", "2025-11-28T14:", "25112814.log", `coll\rphost_1\25112814.log`,
		"12:34.567890-1,CALL,1,timestamp=подделка,event=фейк,file_path=чужой\r\n"},
}

// buildDirect — прямой путь: ParseEventFields → Build.
func buildDirect(t *testing.T, b *RowBuilder, ev []byte, datePrefix, filename, filePath string) Row {
	t.Helper()
	fld, ok := parser.ParseEventFields(ev)
	if !ok {
		t.Fatalf("прямой путь отверг событие (тест-кейс должен быть валидным): %q", ev)
	}
	return b.Build(fld, datePrefix, filename, filePath)
}

// buildViaNDJSON — путь буфера: AppendEvent → BuildNDJSON.
func buildViaNDJSON(t *testing.T, b *RowBuilder, ev []byte, datePrefix, filename, filePath string) Row {
	t.Helper()
	fnEsc := parser.AppendEscaped(nil, []byte(filename))
	fpEsc := parser.AppendEscaped(nil, []byte(filePath))
	line, ok := parser.AppendEvent(nil, ev, datePrefix, fnEsc, fpEsc)
	if !ok {
		t.Fatalf("AppendEvent отверг событие: %q", ev)
	}
	row, err := b.BuildNDJSON(line)
	if err != nil {
		t.Fatalf("BuildNDJSON: %v\nNDJSON: %s", err, line)
	}
	return row
}

func TestBuildNDJSONMirrorsBuild(t *testing.T) {
	modes := []struct {
		name                    string
		rich, sqlNorm, ctxSmart bool
	}{
		{"bench", false, false, false},
		{"rich", true, true, true},
		{"rich_без_sqlnorm", true, false, true},
		{"rich_без_скд", true, true, false},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			for _, tc := range mirrorEvents {
				t.Run(tc.name, func(t *testing.T) {
					// Отдельные билдеры: интернирование не влияет на равенство,
					// но чистое состояние скретчей честнее.
					direct := buildDirect(t, NewRowBuilder(m.rich, m.sqlNorm, m.ctxSmart),
						[]byte(tc.ev), tc.datePrefix, tc.filename, tc.filePath)
					replay := buildViaNDJSON(t, NewRowBuilder(m.rich, m.sqlNorm, m.ctxSmart),
						[]byte(tc.ev), tc.datePrefix, tc.filename, tc.filePath)
					if !reflect.DeepEqual(direct, replay) {
						t.Fatalf("реплей разошёлся с прямым путём\nпрямой: %#v\nреплей: %#v", direct, replay)
					}
					if direct.Rich != nil && !reflect.DeepEqual(*direct.Rich, *replay.Rich) {
						t.Fatalf("RichExt разошёлся\nпрямой: %#v\nреплей: %#v", *direct.Rich, *replay.Rich)
					}
					if direct.bytes != replay.bytes {
						t.Fatalf("учёт bytes разошёлся: %d != %d", direct.bytes, replay.bytes)
					}
				})
			}
		})
	}
}

// TestBuildNDJSONCollectionMatches — вывод collection из file_path при реплее
// идентичен прямому пути (проверка «minimal meta» из ADR: NDJSON достаточен).
func TestBuildNDJSONCollectionMatches(t *testing.T) {
	b := NewRowBuilder(true, true, true)
	for _, fp := range []string{`coll\rphost_1\a.log`, `x/y/z.log`, `plain.log`, ``} {
		line, ok := parser.AppendEvent(nil, []byte("00:01.000001-1,CALL,1,Usr=x\r\n"),
			"2025-11-28T14:", parser.AppendEscaped(nil, []byte("a.log")), parser.AppendEscaped(nil, []byte(fp)))
		if !ok {
			t.Fatal("AppendEvent")
		}
		row, err := b.BuildNDJSON(line)
		if err != nil {
			t.Fatal(err)
		}
		if row.Rich.Collection != CollectionOf(fp) {
			t.Fatalf("collection %q != CollectionOf(%q)=%q", row.Rich.Collection, fp, CollectionOf(fp))
		}
	}
}

// TestBuildNDJSONErrors — битые строки дают ошибку, а не панику/мусор.
func TestBuildNDJSONErrors(t *testing.T) {
	b := NewRowBuilder(false, false, false)
	bad := []string{
		"",
		"{",
		`{"timestamp":"12:34.567890"`,
		`{"нет":"заголовка"}`,
		`{"timestamp":"12:34.567890","duration":x}`,
		`{"timestamp":"12:34.567890","duration":1,"event":"E","level":1,"filename":"f","file_path":"p"`,
		`{"timestamp":"12:34.567890","duration":1,"event":"E","level":1,"filename":"f","file_path":"p",}`,
		`{"timestamp":"12:34.567890","duration":1,"event":"E","level":1,"filename":"f","file_path":"p","a":}`,
		`{"timestamp":"12:34.567890","duration":1,"event":"E","level":1,"filename":"f","file_path":"p","a":"\q"}`,
		`{"timestamp":"12:34.567890","duration":1,"event":"E","level":1,"filename":"f","file_path":"p","a":"\ud800"}`,
		`{"timestamp":"короткий","duration":1,"event":"E","level":1,"filename":"f","file_path":"p"}`,
		"{\"timestamp\":\"12:34.567890\",\"duration\":1,\"event\":\"E\",\"level\":1,\"filename\":\"f\",\"file_path\":\"p\"}\nхвост",
	}
	for i, s := range bad {
		if _, err := b.BuildNDJSON([]byte(s)); err == nil {
			t.Fatalf("битая строка %d принята без ошибки: %q", i, s)
		}
	}
	// Валидная строка с \u-escape (управляющий байт, как пишет AppendEscaped)
	// и суррогатной парой — декодируется.
	okLine := `{"timestamp":"12:34.567890","duration":1,"event":"E","level":1,"filename":"f","file_path":"p","a":"` +
		"\\u0001\\ud83d\\ude00x" + `"}` + "\n"
	row, err := b.BuildNDJSON([]byte(okLine))
	if err != nil {
		t.Fatalf("валидная строка отвергнута: %v", err)
	}
	if row.Props[0].Value != "\x01\U0001F600x" {
		t.Fatalf("декод \\u: %q", row.Props[0].Value)
	}
}

// TestBuildNDJSONScratchReuse — скретчи декодера не «протекают» между
// свойствами и вызовами: значения копируются потребителями.
func TestBuildNDJSONScratchReuse(t *testing.T) {
	b := NewRowBuilder(false, false, false)
	ev1 := "12:34.567890-1,CALL,1,A='первое '' значение',B='второе '' значение'\r\n"
	ev2 := "12:35.567890-2,CALL,1,C='третье '' значение'\r\n"
	mk := func(ev string) Row {
		line, ok := parser.AppendEvent(nil, []byte(ev), "2025-11-28T14:",
			parser.AppendEscaped(nil, []byte("f.log")), parser.AppendEscaped(nil, []byte("p\\f.log")))
		if !ok {
			t.Fatal("AppendEvent")
		}
		r, err := b.BuildNDJSON(line)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r1 := mk(ev1)
	r2 := mk(ev2)
	if r1.Props[0].Value != "первое ' значение" || r1.Props[1].Value != "второе ' значение" {
		t.Fatalf("значения затёрты скретчем: %+v", r1.Props)
	}
	if r2.Props[0].Value != "третье ' значение" {
		t.Fatalf("повторный вызов затёр значения: %+v", r2.Props)
	}
	if strings.Contains(r1.Props[0].Value, "третье") {
		t.Fatal("алиасинг скретча между вызовами")
	}
}
