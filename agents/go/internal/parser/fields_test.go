package parser

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reconstructNDJSON собирает NDJSON-строку из ParseEventFields+ScanProps по
// правилам format-spec (типизация уровня и бескавычечных значений, канонизация
// duration). Совпадение с AppendEvent байт-в-байт — доказательство зеркальности
// обоих путей разбора.
func reconstructNDJSON(ev []byte, datePrefix string, fnEsc, fpEsc []byte) ([]byte, bool) {
	f, ok := ParseEventFields(ev)
	if !ok {
		return nil, false
	}
	dur := f.Duration
	for len(dur) > 1 && dur[0] == '0' {
		dur = dur[1:]
	}
	dst := append([]byte(nil), `{"timestamp":"`...)
	dst = append(dst, datePrefix...)
	dst = append(dst, f.TimePart...)
	dst = append(dst, `","duration":`...)
	dst = append(dst, dur...)
	dst = append(dst, `,"event":"`...)
	dst = AppendEscaped(dst, f.Event)
	dst = append(dst, `","level":`...)
	if IsNumberToken(f.Level) {
		dst = append(dst, f.Level...)
	} else {
		dst = append(dst, '"')
		dst = AppendEscaped(dst, f.Level)
		dst = append(dst, '"')
	}
	dst = append(dst, `,"filename":"`...)
	dst = append(dst, fnEsc...)
	dst = append(dst, `","file_path":"`...)
	dst = append(dst, fpEsc...)
	dst = append(dst, '"')
	var scratch []byte
	scratch = ScanProps(f.Body, f.PropsAt, scratch, func(name, value []byte, quoted bool) {
		dst = append(dst, ',', '"')
		dst = AppendEscaped(dst, name)
		dst = append(dst, '"', ':')
		if !quoted && !isAlwaysStringField(name) && IsNumberToken(value) {
			dst = append(dst, value...)
		} else {
			dst = append(dst, '"')
			dst = AppendEscaped(dst, value)
			dst = append(dst, '"')
		}
	})
	_ = scratch
	dst = append(dst, '}', '\n')
	return dst, true
}

func checkMirror(t *testing.T, label string, ev []byte, datePrefix string) {
	t.Helper()
	fn := AppendEscaped(nil, []byte("25113012.log"))
	fp := AppendEscaped(nil, []byte(`input\rphost_1\25113012.log`))
	want, wantOK := AppendEvent(nil, ev, datePrefix, fn, fp)
	got, gotOK := reconstructNDJSON(ev, datePrefix, fn, fp)
	if wantOK != gotOK {
		t.Errorf("%s: ok расходится: AppendEvent=%v, ParseEventFields=%v (вход %q)", label, wantOK, gotOK, ev)
		return
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s: расхождение путей разбора на входе %q\nAppendEvent:  %s\nиз полей:     %s", label, ev, want, got)
	}
}

func TestFieldsMirrorAppendEvent(t *testing.T) {
	cases := []string{
		// Базовые
		"00:04.000004-007,CALL,1,Usr=test,Ver=8.3.22.1704\r\n",
		"00:04.000004-1,CALL,1\n",
		"00:04.000004-0,EXCP,\r\n",
		"00:04.000004-0,EXCP,,\n",
		// Level съедает остаток (short_header)
		"00:04.000004-5,EXCP,Pad=xxx",
		// Кавычки: удвоение, незакрытые, KI-10
		"00:04.000004-1,CALL,1,A='x''y',B=\"q\"\"w\"",
		"00:04.000004-1,CALL,1,C='unclosed",
		"00:04.000004-1,CALL,1,C=\"unclosed",
		"00:04.000004-1,CALL,1,D='a'garbage'b',E=1",
		"00:04.000004-1,CALL,1,D='x''",
		"00:04.000004-1,CALL,1,D=\"x\"\"",
		"00:04.000004-1,CALL,1,D=''",
		"00:04.000004-1,CALL,1,D=\"\"",
		"00:04.000004-1,CALL,1,D='',E=''",
		// Многострочные значения (реальные \r\n внутри)
		"00:04.000004-1,TLOCK,1,Context='line1\r\nline2\nline3',Next=2",
		"00:04.000004-1,EXCP,0,Descr=\"多行\r\nвторая строка\"",
		// Пустые значения, хвост без '=', приклейка сегмента к ключу
		"00:04.000004-1,CALL,1,Name=",
		"00:04.000004-1,CALL,1,F=,G=2",
		"00:04.000004-1,CALL,1,H=1,tail-no-equals",
		"00:04.000004-1,CALL,1,Foo,Bar=1",
		// Типизация: числа, always-string, дубликаты, spec-поля как имена свойств
		"00:04.000004-1,CALL,1,N=42,M=-1.25E-3,V=007,W=1-2,X=.5",
		"00:04.000004-1,CALL,1,SearchString=123,Guid=42,UUID=0",
		"00:04.000004-1,CALL,1,Dup=1,Dup=2,Dup='три'",
		"00:04.000004-1,CALL,1,event=FAKE,level=9,timestamp=x",
		// Не-ASCII, управляющие байты, экранирование JSON
		"00:04.000004-1,CALL,1,p:processName=сервер_УТ,Опис='кавычка \" внутри'",
		"00:04.000004-1,CALL,1,Ctl='a\tb\x01c\\d'",
		// Уровень-строка и уровень-число на границе грамматики
		"00:04.000004-1,CALL,00,Usr=a",
		"00:04.000004-1,CALL,1e10,Usr=a",
		// Отбраковка (parse_skip)
		"00:04.000004-1,CALL",
		"\r\n",
		"nocomma",
		"no-dash-before,comma",
	}
	for _, c := range cases {
		checkMirror(t, "case", []byte(c), "2025-11-30T12:")
		checkMirror(t, "case-degraded", []byte(c), "") // деградированный timestamp
	}
}

// TestFieldsMirrorGoldenInputs гоняет дифференциальную проверку по всем
// событиям всех golden-кейсов репозитория (реальные фрагменты корпуса).
func TestFieldsMirrorGoldenInputs(t *testing.T) {
	casesDir := filepath.Join("..", "..", "..", "..", "tests", "golden", "cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Skipf("golden-кейсы недоступны (%v) — пропуск", err)
	}
	events := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inputDir := filepath.Join(casesDir, e.Name(), "input")
		logs, _ := filepath.Glob(filepath.Join(inputDir, "*.log"))
		nested, _ := filepath.Glob(filepath.Join(inputDir, "*", "*.log"))
		logs = append(logs, nested...)
		if len(logs) == 0 {
			continue
		}
		for _, lf := range logs {
			data, err := os.ReadFile(lf)
			if err != nil {
				t.Fatalf("чтение %s: %v", lf, err)
			}
			prefix := DateFromFilename(filepath.Base(lf))
			SplitEvents(data, func(ev []byte) {
				checkMirror(t, e.Name()+"/"+filepath.Base(lf), ev, prefix)
				events++
			})
		}
	}
	if events == 0 {
		t.Skip("в golden-кейсах не нашлось событий — пропуск")
	}
	t.Logf("дифференциально сверено событий: %d", events)
}

func TestScanPropsRawValues(t *testing.T) {
	type kv struct {
		name, value string
		quoted      bool
	}
	cases := []struct {
		in   string // свойства (без заголовка)
		want []kv
	}{
		{"Usr=test,Ver=8.3.22.1704", []kv{{"Usr", "test", false}, {"Ver", "8.3.22.1704", false}}},
		{"A='x''y'", []kv{{"A", "x'y", true}}},
		{`B="q""w"`, []kv{{"B", `q"w`, true}}},
		{"C='unclosed", []kv{{"C", "unclosed", true}}},
		{"D='a'garbage'b',E=1", []kv{{"D", "a'garbage'b", true}, {"E", "1", false}}},
		{"D='x''", []kv{{"D", "x'", true}}},
		{"Ctx='line1\r\nline2',N=2", []kv{{"Ctx", "line1\r\nline2", true}, {"N", "2", false}}},
		{"Name=", []kv{{"Name", "", false}}},
		{"F=,G=2", []kv{{"F", "", false}, {"G", "2", false}}},
		{"H=1,tail", []kv{{"H", "1", false}}},
		{"Foo,Bar=1", []kv{{"Foo,Bar", "1", false}}},
	}
	var scratch []byte
	for _, c := range cases {
		ev := []byte("00:04.000004-1,CALL,1," + c.in)
		f, ok := ParseEventFields(ev)
		if !ok {
			t.Fatalf("ParseEventFields(%q) отбросил событие", ev)
		}
		var got []kv
		scratch = ScanProps(f.Body, f.PropsAt, scratch, func(name, value []byte, quoted bool) {
			got = append(got, kv{string(name), string(value), quoted})
		})
		if len(got) != len(c.want) {
			t.Errorf("%q: %d свойств, ожидалось %d (%+v)", c.in, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: свойство %d = %+v, ожидалось %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseEventFieldsHeader(t *testing.T) {
	f, ok := ParseEventFields([]byte("16:06.904004-1500,CONN,2,process=rphost\r\n"))
	if !ok {
		t.Fatal("событие отброшено")
	}
	if string(f.TimePart) != "16:06.904004" || string(f.Duration) != "1500" ||
		string(f.Event) != "CONN" || string(f.Level) != "2" {
		t.Errorf("заголовок разобран неверно: %+v", f)
	}
	if string(f.Body[f.PropsAt:]) != "process=rphost" {
		t.Errorf("PropsAt указывает не на свойства: %q", f.Body[f.PropsAt:])
	}
	if strings.Contains(string(f.Body), "\r") {
		t.Error("хвостовые \\r\\n не обрезаны")
	}
}
