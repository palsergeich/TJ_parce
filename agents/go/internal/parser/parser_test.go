package parser

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDateFromFilename(t *testing.T) {
	cases := map[string]string{
		"25113021.log": "2025-11-30T21:",
		"notadate.log": "",
		"2511302.log":  "",               // 7 цифр + '.', 8-й символ не цифра
		"25133021.log": "2025-13-30T21:", // месяц 13 не валидируется (спека §3)
		"short":        "",
	}
	for in, want := range cases {
		if got := DateFromFilename(in); got != want {
			t.Errorf("DateFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsEventStart(t *testing.T) {
	yes := []string{"00:53.520012-0,", "10:00.000000-5,мусор", "23:59.999999-17500000000,CALL"}
	no := []string{"0:53.520012-0,", "00:53.520012-,", "00:53.520012-0", "00:53.52001a-0,", "строка"}
	for _, s := range yes {
		if !IsEventStart([]byte(s)) {
			t.Errorf("IsEventStart(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsEventStart([]byte(s)) {
			t.Errorf("IsEventStart(%q) = true, want false", s)
		}
	}
}

func TestIsNumberToken(t *testing.T) {
	yes := []string{"0", "42", "-5", "0.5", "1e10", "-1.25E-3", "17500000000"}
	no := []string{"", "007", "8.3.22.1704", "1-2", ".5", "0.", "1.2.3", "-", "+1", "1e", "1e+",
		"123456789012345678901234567890123"} // 33 символа
	for _, s := range yes {
		if !IsNumberToken([]byte(s)) {
			t.Errorf("IsNumberToken(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsNumberToken([]byte(s)) {
			t.Errorf("IsNumberToken(%q) = true, want false", s)
		}
	}
}

func TestAppendEscaped(t *testing.T) {
	type kv struct{ in, want string }
	cases := []kv{
		{"plain", "plain"},
		{"a\"b\\c", `a\"b\\c`},
		{"x\ny\r\tz", `x\ny\r\tz`},
		{"", `\u0001\u001f`},           // управляющие < 0x20 → \u00xx
		{"кириллица ок", "кириллица ок"}, // байты ≥ 0x20 как есть
	}
	for _, c := range cases {
		if got := string(AppendEscaped(nil, []byte(c.in))); got != c.want {
			t.Errorf("AppendEscaped(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAppendEventBasics(t *testing.T) {
	fn := AppendEscaped(nil, []byte("25113012.log"))
	fp := AppendEscaped(nil, []byte(`input\rphost_1\25113012.log`))

	got, ok := AppendEvent(nil, []byte("00:04.000004-007,CALL,1,Usr=test,Ver=8.3.22.1704\r\n"),
		"2025-11-30T12:", fn, fp)
	if !ok {
		t.Fatal("событие отброшено")
	}
	want := `{"timestamp":"2025-11-30T12:00:04.000004","duration":7,"event":"CALL","level_num":1,` +
		`"filename":"25113012.log","file_path":"input\\rphost_1\\25113012.log","Usr":"test","Ver":"8.3.22.1704"}` + "\n"
	if string(got) != want {
		t.Errorf("получено:\n%s\nожидалось:\n%s", got, want)
	}

	// Нет второй запятой → parse_skip
	if _, ok := AppendEvent(nil, []byte("00:04.000004-1,CALL"), "", fn, fp); ok {
		t.Error("событие без второй запятой должно отбрасываться")
	}
}

func TestSplitEventsBOM(t *testing.T) {
	data := []byte("\xEF\xBB\xBF00:04.000004-10,CALL,1,Usr=a\n00:05.000005-20,CALL,2,Usr=b\n")
	var n int
	SplitEvents(data, func(ev []byte) { n++ })
	if n != 2 {
		t.Errorf("после BOM должно быть 2 события, получено %d", n)
	}
}

// TestRepeatedKeysBecomeArray — §4.5 rev 4: ключ, встретившийся в событии
// больше одного раза, кодируется массивом значений в порядке источника;
// одиночные ключи остаются скалярами, порядок ключей — по первому вхождению.
func TestRepeatedKeysBecomeArray(t *testing.T) {
	cases := []struct{ name, props, want string }{
		{
			name:  "две пары Func как в живом ТЖ",
			props: "Trans=0,Func=Transaction,Func=CommitTransaction,Usr=x",
			want:  `"Trans":0,"Func":["Transaction","CommitTransaction"],"Usr":"x"`,
		},
		{
			name:  "три вхождения",
			props: "A=1,A=2,A=3",
			want:  `"A":[1,2,3]`,
		},
		{
			name:  "повтор не подряд, порядок ключей по первому вхождению",
			props: "A=1,B=2,A=3",
			want:  `"A":[1,3],"B":2`,
		},
		{
			name:  "типизация применяется к каждому элементу",
			props: "V=42,V=abc",
			want:  `"V":[42,"abc"]`,
		},
		{
			name:  "без повторов буфер не перестраивается",
			props: "A=1,B=2,C=3",
			want:  `"A":1,"B":2,"C":3`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := AppendEscaped(nil, []byte("25113012.log"))
			fp := AppendEscaped(nil, []byte(`input\rphost_1\25113012.log`))
			ev := []byte("00:01.000001-7,CALL,1," + c.props)
			got, ok := AppendEvent(nil, ev, "2025-11-30T12:", fn, fp)
			if !ok {
				t.Fatalf("событие отброшено: %s", ev)
			}
			if !strings.Contains(string(got), c.want) {
				t.Errorf("получено:\n%s\nожидался фрагмент:\n%s", got, c.want)
			}
			var probe map[string]any
			if err := json.Unmarshal(got[:len(got)-1], &probe); err != nil {
				t.Fatalf("невалидный JSON: %v\n%s", err, got)
			}
		})
	}
}
