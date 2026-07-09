//go:build liveverify

package sqlnorm

// Приёмочный харнесс нормализации на живых данных (НЕ входит в обычный
// go test — только с тегом liveverify). По каждому УНИКАЛЬНОМУ sql_hash
// таблицы берётся сырой текст и сохранённые агентом sql_norm_hash/sql_params,
// нормализация пересчитывается этим пакетом и сверяется бит-в-бит:
//
//	go test -tags liveverify -run TestLiveVerify -v ./internal/sqlnorm/
//
// Переменные окружения:
//
//	TJ_VERIFY_URL   — HTTP-endpoint ClickHouse (по умолчанию http://localhost:8123)
//	TJ_VERIFY_TABLE — таблица rich-схемы (по умолчанию tj_sqlnorm_check.events)
//
// Вместе с SQL-проверками на всей таблице (param_count == length(sql_params);
// один sql_norm_hash на sql_hash) это даёт полное покрытие: каждая строка
// таблицы соответствует пересчитанному здесь уникальному тексту.
import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-faster/city"
)

func TestLiveVerify(t *testing.T) {
	base := os.Getenv("TJ_VERIFY_URL")
	if base == "" {
		base = "http://localhost:8123"
	}
	table := os.Getenv("TJ_VERIFY_TABLE")
	if table == "" {
		table = "tj_sqlnorm_check.events"
	}
	q := "SELECT sql_text AS t, toString(sql_norm_hash) AS h, sql_params AS p " +
		"FROM " + table + " WHERE sql_text != '' LIMIT 1 BY sql_hash " +
		"FORMAT JSONEachRow"
	resp, err := http.Post(base+"/?"+url.Values{"query": {q}}.Encode(), "text/plain", nil)
	if err != nil {
		t.Fatalf("ClickHouse недоступен: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var msg strings.Builder
		_, _ = fmt.Fprintf(&msg, "HTTP %d: ", resp.StatusCode)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() && msg.Len() < 500 {
			msg.WriteString(sc.Text())
		}
		t.Fatal(msg.String())
	}

	type row struct {
		T string   `json:"t"`
		H string   `json:"h"`
		P []string `json:"p"`
	}
	var (
		n        Normalizer
		total    int
		hashErr  int
		paramErr int
		qErr     int
	)
	// TJ_VERIFY_SPOT=1 — напечатать три разнотипных примера (сырец / норма /
	// params) для визуальной сверки в отчёте: большой список, строковый
	// параметр, точечный запрос.
	spot := os.Getenv("TJ_VERIFY_SPOT") != ""
	spotBig, spotStr, spotSmall := false, false, false
	shorten := func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + " …<обрезано>"
	}
	printSpot := func(kind, raw, norm string, params []string) {
		ps := fmt.Sprintf("%q", params)
		t.Logf("\n--- СПОТ (%s) ---\nсырец:  %s\nнорма:  %s\nparams (%d): %s",
			kind, shorten(raw, 600), shorten(norm, 400), len(params), shorten(ps, 400))
	}
	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 4<<20))
	for dec.More() {
		var r row
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("строка %d: %v", total, err)
		}
		total++
		wantHash, err := strconv.ParseUint(r.H, 10, 64)
		if err != nil {
			t.Fatalf("строка %d: sql_norm_hash %q: %v", total, r.H, err)
		}
		norm, params := n.Normalize(r.T)
		if got := city.CH64(norm); got != wantHash {
			hashErr++
			if hashErr <= 3 {
				t.Errorf("хэш нормы разошёлся: got %d, want %d\nтекст: %.200q\nнорма: %.200q",
					got, wantHash, r.T, string(norm))
			}
		}
		if !equalStrings(params, r.P) {
			paramErr++
			if paramErr <= 3 {
				t.Errorf("params разошлись (%d против %d)\nтекст: %.200q\n got: %.10q\nwant: %.10q",
					len(params), len(r.P), r.T, params, r.P)
			}
		}
		if strings.Count(string(norm), "?") > len(params) {
			qErr++
			if qErr <= 3 {
				t.Errorf("'?' в норме больше, чем params: %.200q", string(norm))
			}
		}
		if spot {
			switch {
			case !spotBig && len(params) >= 100:
				spotBig = true
				printSpot("большой список", r.T, string(norm), params)
			case !spotStr && hasWordyParam(params):
				spotStr = true
				printSpot("строковый параметр", r.T, string(norm), params)
			case !spotSmall && len(params) >= 1 && len(params) <= 4 && len(r.T) < 700:
				spotSmall = true
				printSpot("точечный запрос", r.T, string(norm), params)
			}
		}
	}
	if hashErr+paramErr+qErr > 0 {
		t.Fatalf("уникальных текстов: %d; расхождений хэша: %d, params: %d, '?': %d",
			total, hashErr, paramErr, qErr)
	}
	t.Logf("уникальных текстов сверено: %d; расхождений: 0", total)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hasWordyParam — среди значений есть «словесное» (кириллица/пробел —
// строковый литерал, а не hex/число): кандидат наглядного спот-примера.
func hasWordyParam(params []string) bool {
	for _, p := range params {
		if strings.ContainsRune(p, ' ') || strings.IndexFunc(p, func(r rune) bool { return r >= 0x400 }) >= 0 {
			return true
		}
	}
	return false
}
