// row.go — маппинг разобранного события ТЖ в строку таблицы ClickHouse.
//
// Единый контракт всех трёх участников bake-off (см. README §ClickHouse-sink):
//   - timestamp: "20YY-MM-DDTHH:" (из имени файла) + "ММ:СС.мммммм" (из события)
//     → DateTime64(6); время без TZ трактуется как UTC (литерал текста равен
//     значению при серверной TZ UTC). Деградированный timestamp (имя файла не
//     YYMMDDHH.log, пустой префикс) → нулевое время 1970-01-01.
//   - duration → UInt64 (насыщение на переполнении).
//   - event/filename/file_path → строки как в NDJSON; level → строка: числовой
//     уровень — его десятичный текст, строковый — как есть.
//   - props → ВСЕ свойства события, имя → значение-ТЕКСТ: для кавычечных
//     значений содержимое после расклейки удвоенных кавычек (пара апострофов
//     → апостроф, пара "" → ") — ровно то, что в NDJSON стоит внутри
//     JSON-строки до экранирования; для бескавычечных — сырой
//     токен. Многострочные значения сохраняют реальные \r\n. Дубликаты ключей
//     схлопываются «последнее значение побеждает» (format-spec §4.5); порядок
//     свойств события сохраняется в Map.
package chsink

import (
	"math"
	"time"

	"tjagent/internal/parser"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
)

// Pair — одно свойство события.
type Pair struct{ Name, Value string }

// Props — упорядоченный список свойств; реализует column.IterableOrderedMap,
// чтобы Map-колонка получала пары в порядке следования свойств в событии
// (map[string]string дал бы случайный порядок итерации).
type Props []Pair

// Put — часть интерфейса IterableOrderedMap (используется клиентом при Scan).
func (p *Props) Put(key any, value any) {
	k, _ := key.(string)
	v, _ := value.(string)
	*p = append(*p, Pair{k, v})
}

// Iterator — часть интерфейса IterableOrderedMap (используется при вставке).
func (p *Props) Iterator() column.MapIterator { return &propsIter{p: *p, i: -1} }

type propsIter struct {
	p Props
	i int
}

func (it *propsIter) Next() bool { it.i++; return it.i < len(it.p) }
func (it *propsIter) Key() any   { return it.p[it.i].Name }
func (it *propsIter) Value() any { return it.p[it.i].Value }

// Src — происхождение строки для чекпоинтов follow-режима: индекс файла в
// реестре вызывающего, поколение файла (инкрементируется при усечении/
// пересоздании — ack устаревшего поколения игнорируется) и абсолютный оффсет
// файла сразу за последним байтом события. В batch-режиме — нулевое значение.
type Src struct {
	File uint32
	Gen  uint32
	End  int64
}

// Row — строка tj_bench.events (DDL — bakeoff-protocol §1.2).
type Row struct {
	Time     time.Time
	Duration uint64
	Event    string
	Level    string
	Filename string
	FilePath string
	Props    Props
	Src      Src // метка для OnAck (только follow-режим)
	bytes    int // оценка вклада строки в порог BatchBytes
}

// rowFixedBytes — оценка фиксированной части строки (timestamp 8 + duration 8
// + оффсеты Map 8) для порога батча по байтам.
const rowFixedBytes = 24

// EventTime собирает момент события из префикса даты файла (DateFromFilename,
// "20YY-MM-DDTHH:") и времени события "ММ:СС.мммммм". Диапазоны не
// валидируются (спека §3): «месяц 13» time.Date нормализует переносом
// (2025-13 → 2026-01). Любой некондиционный вход → нулевое время 1970-01-01.
func EventTime(datePrefix string, timePart []byte) time.Time {
	if len(datePrefix) != 14 || len(timePart) != 12 {
		return time.Unix(0, 0).UTC()
	}
	year := atoi4(datePrefix[0:4])
	month := atoi2(datePrefix[5:7])
	day := atoi2(datePrefix[8:10])
	hour := atoi2(datePrefix[11:13])
	if year < 0 || month < 0 || day < 0 || hour < 0 {
		return time.Unix(0, 0).UTC()
	}
	// Маска события гарантирует цифры в позициях ММ:СС.мммммм
	min := atoi2b(timePart[0:2])
	sec := atoi2b(timePart[3:5])
	micros := 0
	for _, c := range timePart[6:12] {
		if c < '0' || c > '9' {
			return time.Unix(0, 0).UTC()
		}
		micros = micros*10 + int(c-'0')
	}
	if min < 0 || sec < 0 {
		return time.Unix(0, 0).UTC()
	}
	return time.Date(year, time.Month(month), day, hour, min, sec, micros*1000, time.UTC)
}

func atoi2(s string) int {
	if s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return -1
	}
	return int(s[0]-'0')*10 + int(s[1]-'0')
}

func atoi2b(b []byte) int {
	if b[0] < '0' || b[0] > '9' || b[1] < '0' || b[1] > '9' {
		return -1
	}
	return int(b[0]-'0')*10 + int(b[1]-'0')
}

func atoi4(s string) int {
	hi, lo := atoi2(s[0:2]), atoi2(s[2:4])
	if hi < 0 || lo < 0 {
		return -1
	}
	return hi*100 + lo
}

// ParseDuration — беззнаковый разбор токена длительности (маска гарантирует
// цифры). Переполнение UInt64 насыщается до MaxUint64, нецифровой байт
// обрывает разбор (защита от некондиционного входа).
func ParseDuration(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		d := uint64(c - '0')
		if v > (math.MaxUint64-d)/10 {
			return math.MaxUint64
		}
		v = v*10 + d
	}
	return v
}

// internCap — предохранитель интернирования: при большем числе уникальных
// строк (мусорный корпус) новые перестают запоминаться, копии остаются
// корректными.
const internCap = 4096

// RowBuilder — worker-локальный конструктор строк: интернирование
// низкокардинальных строк (имена событий, уровни, имена свойств) и
// scratch-буфер расклейки кавычек. НЕ потокобезопасен.
type RowBuilder struct {
	names   map[string]string
	scratch []byte
}

func NewRowBuilder() *RowBuilder {
	return &RowBuilder{names: make(map[string]string, 128)}
}

func (b *RowBuilder) intern(s []byte) string {
	if v, ok := b.names[string(s)]; ok { // lookup по string([]byte) без аллокации
		return v
	}
	v := string(s)
	if len(b.names) < internCap {
		b.names[v] = v
	}
	return v
}

// Build превращает разобранное событие в строку таблицы. filename/filePath —
// готовые строки (общие на файл). Значения свойств копируются из буфера
// события (срезы parser валидны только внутри колбэка ScanEvents).
func (b *RowBuilder) Build(f parser.EventFields, datePrefix, filename, filePath string) Row {
	r := Row{
		Time:     EventTime(datePrefix, f.TimePart),
		Duration: ParseDuration(f.Duration),
		Event:    b.intern(f.Event),
		Level:    b.intern(f.Level),
		Filename: filename,
		FilePath: filePath,
	}
	r.bytes = rowFixedBytes + len(r.Event) + len(r.Level) + len(filename) + len(filePath)
	b.scratch = parser.ScanProps(f.Body, f.PropsAt, b.scratch, func(name, value []byte, _ bool) {
		key := b.intern(name)
		val := string(value)
		for i := range r.Props { // дубликат ключа: последнее значение побеждает
			if r.Props[i].Name == key {
				r.bytes += len(val) - len(r.Props[i].Value)
				r.Props[i].Value = val
				return
			}
		}
		r.Props = append(r.Props, Pair{Name: key, Value: val})
		r.bytes += len(key) + len(val)
	})
	return r
}
