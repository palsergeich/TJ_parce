// Package metrics — метрики агента и endpoint /metrics в текстовом формате
// Prometheus (exposition format 0.0.4). Реализация ручная, без client_golang:
// агенту нужны счётчики-атомики, две gauge, одна гистограмма и один HTTP-хэндлер —
// зависимость на полный клиент не окупается (задача из docs/storage-design.md §5).
//
// Инварианты формата: счётчики монотонны (только атомарные инкременты),
// значения лейблов экранируются (\\, \", \n), гистограмма кумулятивна и
// заканчивается бакетом le="+Inf" (= _count). Реестр глобален на процесс:
// у агента ровно один конвейер, горячий путь пишет в атомики без блокировок.
//
// Набор метрик (storage-design §5, состав фазы 3):
//
//	tj_agent_read_bytes_total{collection}   counter — прочитано сырого ТЖ
//	tj_agent_events_total{collection}       counter — нормализовано событий
//	tj_agent_parse_errors_total{collection} counter — записей не прошло разбор
//	tj_agent_lag_seconds{collection}        gauge   — now − max ts события (SLI)
//	tj_agent_files_open                     gauge   — открытых хэндлов .log
//	tj_ingest_batches_total{status}         counter — вставки ok|retried|failed
//	tj_ingest_rows_total                    counter — строк подтверждено сервером
//	tj_ingest_queue_depth                   gauge   — слабов в очереди на вставку
//	tj_ingest_insert_seconds                histogram — латентность попытки INSERT
package metrics

import (
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Coll — счётчики одной коллекции (первый сегмент относительного пути файла,
// он же колонка collection rich-схемы). Указатель кэшируется на горячем пути
// (по одному lookup на файл, не на событие).
type Coll struct {
	name string

	ReadBytes   atomic.Uint64
	Events      atomic.Uint64
	ParseErrors atomic.Uint64

	// maxTSMicro — максимальный ts обработанного события, unix-микросекунды.
	// 0 — валидных событий ещё не было (серия lag не публикуется).
	maxTSMicro atomic.Int64
}

// ObserveEventTS продвигает максимальный ts события коллекции (CAS-max).
// Деградированные метки времени (эпоха и раньше) игнорируются — иначе одна
// битая запись показывала бы lag в десятилетия.
func (c *Coll) ObserveEventTS(t time.Time) {
	us := t.UnixMicro()
	if us <= 0 {
		return
	}
	for {
		cur := c.maxTSMicro.Load()
		if us <= cur || c.maxTSMicro.CompareAndSwap(cur, us) {
			return
		}
	}
}

var (
	collMu sync.RWMutex
	colls  = map[string]*Coll{}

	filesOpen atomic.Int64

	batchesOK      atomic.Uint64
	batchesRetried atomic.Uint64
	batchesFailed  atomic.Uint64
	rowsTotal      atomic.Uint64

	queueMu sync.Mutex
	queueFn func() int

	insertHist = newHistogram(
		// Латентность INSERT: применение блока rich-схемы — сотни мс, деградация
		// при недоступном сервере — до dial-таймаутов в десятки секунд.
		0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
	)
)

// GetColl возвращает (создавая при первом обращении) счётчики коллекции.
func GetColl(name string) *Coll {
	collMu.RLock()
	c := colls[name]
	collMu.RUnlock()
	if c != nil {
		return c
	}
	collMu.Lock()
	defer collMu.Unlock()
	if c = colls[name]; c == nil {
		c = &Coll{name: name}
		colls[name] = c
	}
	return c
}

// FilesOpenAdd — дельта gauge tj_agent_files_open (+1 открытие, −1 закрытие).
func FilesOpenAdd(d int64) { filesOpen.Add(d) }

// BatchOK — батч подтверждён с первой попытки.
func BatchOK() { batchesOK.Add(1) }

// BatchRetried — батч подтверждён после ≥1 повтора (follow-режим).
func BatchRetried() { batchesRetried.Add(1) }

// BatchFailed — неудачная попытка вставки (каждая: алерт
// rate(tj_ingest_batches_total{status="failed"}[5m]) > 0 ловит недоступность
// сервера сразу, а не после исчерпания повторов).
func BatchFailed() { batchesFailed.Add(1) }

// AddRows — строк подтверждено сервером.
func AddRows(n uint64) { rowsTotal.Add(n) }

// ObserveInsertSeconds — длительность одной попытки INSERT (успех и ошибка).
func ObserveInsertSeconds(s float64) { insertHist.observe(s) }

// SetQueueDepthFunc устанавливает сэмплер tj_ingest_queue_depth (обычно
// len(канала слабов) действующего sink'а); nil — снять (глубина 0).
func SetQueueDepthFunc(f func() int) {
	queueMu.Lock()
	queueFn = f
	queueMu.Unlock()
}

func queueDepth() int {
	queueMu.Lock()
	f := queueFn
	queueMu.Unlock()
	if f == nil {
		return 0
	}
	return f()
}

// histogram — кумулятивная гистограмма с фиксированными границами.
type histogram struct {
	bounds  []float64
	counts  []atomic.Uint64 // len(bounds)+1, последний — (последняя граница, +Inf]
	sumBits atomic.Uint64   // float64 через CAS битов
	count   atomic.Uint64
}

func newHistogram(bounds ...float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // первый bound >= v
	h.counts[i].Add(1)
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		neu := math.Float64bits(math.Float64frombits(old) + v)
		if h.sumBits.CompareAndSwap(old, neu) {
			return
		}
	}
}

// --- Рендер текстового формата -------------------------------------------

// escapeLabel — экранирование значения лейбла (спека формата: \\, \", \n).
var escapeLabel = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeHelp — экранирование HELP-строки (\\ и \n).
var escapeHelp = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

type renderer struct {
	w   io.Writer
	buf []byte
}

func (r *renderer) header(name, help, typ string) {
	r.buf = r.buf[:0]
	r.buf = append(r.buf, "# HELP "...)
	r.buf = append(r.buf, name...)
	r.buf = append(r.buf, ' ')
	r.buf = append(r.buf, escapeHelp.Replace(help)...)
	r.buf = append(r.buf, "\n# TYPE "...)
	r.buf = append(r.buf, name...)
	r.buf = append(r.buf, ' ')
	r.buf = append(r.buf, typ...)
	r.buf = append(r.buf, '\n')
	_, _ = r.w.Write(r.buf)
}

// sample печатает одну строку метрики; labels — чередование имя, значение.
func (r *renderer) sample(name string, value string, labels ...string) {
	r.buf = r.buf[:0]
	r.buf = append(r.buf, name...)
	if len(labels) > 0 {
		r.buf = append(r.buf, '{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				r.buf = append(r.buf, ',')
			}
			r.buf = append(r.buf, labels[i]...)
			r.buf = append(r.buf, `="`...)
			r.buf = append(r.buf, escapeLabel.Replace(labels[i+1])...)
			r.buf = append(r.buf, '"')
		}
		r.buf = append(r.buf, '}')
	}
	r.buf = append(r.buf, ' ')
	r.buf = append(r.buf, value...)
	r.buf = append(r.buf, '\n')
	_, _ = r.w.Write(r.buf)
}

func fuint(v uint64) string { return strconv.FormatUint(v, 10) }
func ffloat(v float64) string {
	if math.IsInf(v, +1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// RenderText пишет полный снапшот реестра в текстовом формате Prometheus.
// now — момент вычисления lag (инъекция для тестов).
func RenderText(w io.Writer, now time.Time) {
	r := &renderer{w: w, buf: make([]byte, 0, 256)}

	collMu.RLock()
	names := make([]string, 0, len(colls))
	for n := range colls {
		names = append(names, n)
	}
	sort.Strings(names)
	snap := make([]*Coll, len(names))
	for i, n := range names {
		snap[i] = colls[n]
	}
	collMu.RUnlock()

	r.header("tj_agent_read_bytes_total", "Прочитано байт сырого техжурнала.", "counter")
	for _, c := range snap {
		r.sample("tj_agent_read_bytes_total", fuint(c.ReadBytes.Load()), "collection", c.name)
	}

	r.header("tj_agent_events_total", "Нормализовано событий техжурнала.", "counter")
	for _, c := range snap {
		r.sample("tj_agent_events_total", fuint(c.Events.Load()), "collection", c.name)
	}

	r.header("tj_agent_parse_errors_total", "Записей, не прошедших разбор (parse_skips).", "counter")
	for _, c := range snap {
		r.sample("tj_agent_parse_errors_total", fuint(c.ParseErrors.Load()), "collection", c.name)
	}

	r.header("tj_agent_lag_seconds", "Отставание обработки: часы источника (локальное время как UTC, конвенция хранилища) минус максимальный ts события. Главный SLI.", "gauge")
	for _, c := range snap {
		us := c.maxTSMicro.Load()
		if us <= 0 {
			continue // валидных событий ещё не было
		}
		lag := now.Sub(time.UnixMicro(us)).Seconds()
		if lag < 0 {
			lag = 0 // часы источника впереди скрейпера
		}
		r.sample("tj_agent_lag_seconds", ffloat(lag), "collection", c.name)
	}

	r.header("tj_agent_files_open", "Открытых хэндлов .log-хвостов.", "gauge")
	fo := filesOpen.Load()
	if fo < 0 {
		fo = 0
	}
	r.sample("tj_agent_files_open", strconv.FormatInt(fo, 10))

	r.header("tj_ingest_batches_total", "Батчи вставки: ok - с первой попытки, retried - после повторов, failed - неудачные попытки.", "counter")
	r.sample("tj_ingest_batches_total", fuint(batchesOK.Load()), "status", "ok")
	r.sample("tj_ingest_batches_total", fuint(batchesRetried.Load()), "status", "retried")
	r.sample("tj_ingest_batches_total", fuint(batchesFailed.Load()), "status", "failed")

	r.header("tj_ingest_rows_total", "Строк, подтверждённых сервером БД.", "counter")
	r.sample("tj_ingest_rows_total", fuint(rowsTotal.Load()))

	r.header("tj_ingest_queue_depth", "Слабов строк в очереди на вставку.", "gauge")
	r.sample("tj_ingest_queue_depth", strconv.Itoa(queueDepth()))

	r.header("tj_ingest_insert_seconds", "Латентность одной попытки INSERT (успехи и ошибки).", "histogram")
	var cum uint64
	for i, b := range insertHist.bounds {
		cum += insertHist.counts[i].Load()
		r.sample("tj_ingest_insert_seconds_bucket", fuint(cum), "le", ffloat(b))
	}
	cum += insertHist.counts[len(insertHist.bounds)].Load()
	r.sample("tj_ingest_insert_seconds_bucket", fuint(cum), "le", "+Inf")
	r.sample("tj_ingest_insert_seconds_sum", ffloat(math.Float64frombits(insertHist.sumBits.Load())))
	r.sample("tj_ingest_insert_seconds_count", fuint(insertHist.count.Load()))
}

// resetForTest — чистый реестр (только тесты пакета).
func resetForTest() {
	collMu.Lock()
	colls = map[string]*Coll{}
	collMu.Unlock()
	filesOpen.Store(0)
	batchesOK.Store(0)
	batchesRetried.Store(0)
	batchesFailed.Store(0)
	rowsTotal.Store(0)
	SetQueueDepthFunc(nil)
	for i := range insertHist.counts {
		insertHist.counts[i].Store(0)
	}
	insertHist.sumBits.Store(0)
	insertHist.count.Store(0)
}
