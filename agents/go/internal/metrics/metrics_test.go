package metrics

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRenderTextFormat — сквозная проверка exposition-формата: значения,
// экранирование лейблов, gauge/counter/histogram-семейства.
func TestRenderTextFormat(t *testing.T) {
	resetForTest()
	defer resetForTest()

	// Имя коллекции с полным набором спецсимволов формата: \ " \n
	c := GetColl("Di\"ag\\x\ny")
	c.ReadBytes.Add(100)
	c.Events.Add(5)
	c.ParseErrors.Add(1)

	now := time.Unix(1_000_000, 0).UTC()
	c.ObserveEventTS(now.Add(-2 * time.Second))

	FilesOpenAdd(1)
	BatchOK()
	BatchRetried()
	BatchFailed()
	AddRows(50)
	ObserveInsertSeconds(0.02) // бакет 0.025
	ObserveInsertSeconds(100)  // за последней границей → только +Inf
	SetQueueDepthFunc(func() int { return 7 })

	var b bytes.Buffer
	RenderText(&b, now)
	out := b.String()

	for _, want := range []string{
		"# TYPE tj_agent_read_bytes_total counter\n",
		`tj_agent_read_bytes_total{collection="Di\"ag\\x\ny"} 100` + "\n",
		`tj_agent_events_total{collection="Di\"ag\\x\ny"} 5` + "\n",
		`tj_agent_parse_errors_total{collection="Di\"ag\\x\ny"} 1` + "\n",
		"# TYPE tj_agent_lag_seconds gauge\n",
		`tj_agent_lag_seconds{collection="Di\"ag\\x\ny"} 2` + "\n",
		"tj_agent_files_open 1\n",
		`tj_ingest_batches_total{status="ok"} 1` + "\n",
		`tj_ingest_batches_total{status="retried"} 1` + "\n",
		`tj_ingest_batches_total{status="failed"} 1` + "\n",
		"tj_ingest_rows_total 50\n",
		"tj_ingest_queue_depth 7\n",
		"# TYPE tj_ingest_insert_seconds histogram\n",
		`tj_ingest_insert_seconds_bucket{le="0.01"} 0` + "\n",
		`tj_ingest_insert_seconds_bucket{le="0.025"} 1` + "\n",
		`tj_ingest_insert_seconds_bucket{le="60"} 1` + "\n",
		`tj_ingest_insert_seconds_bucket{le="+Inf"} 2` + "\n",
		"tj_ingest_insert_seconds_sum 100.02\n",
		"tj_ingest_insert_seconds_count 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q\n--- вывод ---\n%s", want, out)
		}
	}

	// Структурная валидация: каждая строка — комментарий или сэмпл вида
	// name[{...}] value, имена — из ожидаемого словаря.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp <= 0 {
			t.Errorf("строка без значения: %q", line)
			continue
		}
		name := line[:sp]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			if !strings.HasSuffix(name, "}") {
				t.Errorf("незакрытые лейблы: %q", line)
			}
			name = name[:i]
		}
		if !strings.HasPrefix(name, "tj_agent_") && !strings.HasPrefix(name, "tj_ingest_") {
			t.Errorf("неожиданное имя метрики: %q", line)
		}
	}
}

// TestCountersMonotonic — повторный рендер после инкрементов не уменьшает
// значения (контракт counter).
func TestCountersMonotonic(t *testing.T) {
	resetForTest()
	defer resetForTest()

	c := GetColl("Mono")
	c.Events.Add(3)
	BatchOK()

	grab := func() (string, string) {
		var b bytes.Buffer
		RenderText(&b, time.Unix(1, 0))
		var ev, ok string
		for _, l := range strings.Split(b.String(), "\n") {
			if strings.HasPrefix(l, `tj_agent_events_total{collection="Mono"} `) {
				ev = l
			}
			if strings.HasPrefix(l, `tj_ingest_batches_total{status="ok"} `) {
				ok = l
			}
		}
		return ev, ok
	}
	ev1, ok1 := grab()
	c.Events.Add(2)
	BatchOK()
	ev2, ok2 := grab()
	if ev1 != `tj_agent_events_total{collection="Mono"} 3` || ev2 != `tj_agent_events_total{collection="Mono"} 5` {
		t.Errorf("events: %q -> %q", ev1, ev2)
	}
	if ok1 != `tj_ingest_batches_total{status="ok"} 1` || ok2 != `tj_ingest_batches_total{status="ok"} 2` {
		t.Errorf("batches ok: %q -> %q", ok1, ok2)
	}
}

// TestLagSeries — эпоха игнорируется (серии нет), будущее время зажимается в 0,
// CAS-max не откатывается назад.
func TestLagSeries(t *testing.T) {
	resetForTest()
	defer resetForTest()

	now := time.Unix(2_000_000, 0).UTC()
	render := func() string {
		var b bytes.Buffer
		RenderText(&b, now)
		return b.String()
	}

	c := GetColl("L")
	c.ObserveEventTS(time.Unix(0, 0)) // эпоха — деградированный ts
	if out := render(); strings.Contains(out, `tj_agent_lag_seconds{collection="L"}`) {
		t.Errorf("эпоха не должна публиковать lag:\n%s", out)
	}

	c.ObserveEventTS(now.Add(-10 * time.Second))
	c.ObserveEventTS(now.Add(-30 * time.Second)) // назад — игнор (max)
	if out := render(); !strings.Contains(out, `tj_agent_lag_seconds{collection="L"} 10`+"\n") {
		t.Errorf("ожидался lag 10:\n%s", out)
	}

	c.ObserveEventTS(now.Add(5 * time.Second)) // часы источника впереди
	if out := render(); !strings.Contains(out, `tj_agent_lag_seconds{collection="L"} 0`+"\n") {
		t.Errorf("ожидался lag 0 (зажим будущего):\n%s", out)
	}
}

// TestHistogramCumulative — бакеты неубывающие, +Inf == _count.
func TestHistogramCumulative(t *testing.T) {
	resetForTest()
	defer resetForTest()

	for _, v := range []float64{0.001, 0.02, 0.3, 4, 59, 61, 1000} {
		ObserveInsertSeconds(v)
	}
	var b bytes.Buffer
	RenderText(&b, time.Unix(1, 0))
	prev := int64(-1)
	var inf, count int64
	val := func(l string) int64 {
		v, err := strconv.ParseInt(l[strings.LastIndexByte(l, ' ')+1:], 10, 64)
		if err != nil {
			t.Fatalf("не целое значение: %q", l)
		}
		return v
	}
	for _, l := range strings.Split(b.String(), "\n") {
		switch {
		case strings.HasPrefix(l, "tj_ingest_insert_seconds_bucket{"):
			v := val(l)
			if v < prev {
				t.Errorf("бакеты не кумулятивны: %q после %d", l, prev)
			}
			prev = v
			if strings.Contains(l, `le="+Inf"`) {
				inf = v
			}
		case strings.HasPrefix(l, "tj_ingest_insert_seconds_count "):
			count = val(l)
		}
	}
	if inf != 7 || count != 7 {
		t.Errorf("+Inf=%d count=%d, ожидалось 7/7", inf, count)
	}
}
