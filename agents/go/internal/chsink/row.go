// row.go — маппинг разобранного события ТЖ в строку таблицы ClickHouse.
//
// Bench-схема (единый контракт всех трёх участников bake-off, README
// §ClickHouse-sink):
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
//
// Rich-схема (продуктовая tj.events): семантика импортёра — см. rich.go.
// Отличия от bench в одной строке: горячие колонки берут ПЕРВОЕ вхождение
// ключа, props-хвост сохраняет дубликаты, ts валидируется, duration без
// насыщения.
package chsink

import (
	"math"
	"time"

	"tjagent/internal/parser"
	"tjagent/internal/sqlnorm"
)

// Pair — одно свойство события.
type Pair struct{ Name, Value string }

// Props — упорядоченный список свойств (порядок следования в событии).
type Props []Pair

// Src — происхождение строки для чекпоинтов follow-режима: индекс файла в
// реестре вызывающего, поколение файла (инкрементируется при усечении/
// пересоздании — ack устаревшего поколения игнорируется) и абсолютный оффсет
// файла сразу за последним байтом события. В batch-режиме — нулевое значение.
type Src struct {
	File uint32
	Gen  uint32
	End  int64
}

// Row — строка целевой таблицы. В bench-режиме заполняется базовая часть
// (Props = все свойства, дедуп last-wins); в rich-режиме Props — хвост
// невыбранных свойств (с дубликатами), а горячие колонки лежат в Rich.
type Row struct {
	Time     time.Time
	Duration uint64
	Event    string
	Level    string
	Filename string
	FilePath string
	Props    Props
	Rich     *RichExt // nil в bench-режиме
	Src      Src      // метка для OnAck (только follow-режим)
	bytes    int      // оценка вклада строки в порог BatchBytes
}

// rowFixedBytes — оценка фиксированной части строки (timestamp 8 + duration 8
// + оффсеты Map 8) для порога батча по байтам.
const rowFixedBytes = 24

// richFixedBytes — добавка фиксированных полей rich-схемы (ts, числовые
// колонки, хэши) к оценке r.bytes.
const richFixedBytes = 26*8 + 16

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
// rich=true переключает на маппинг продуктовой схемы (rich.go);
// sqlNorm=true (только с rich) дополнительно считает нормализацию SQL
// (sql_norm_hash / param_count / sql_params, docs/sql-normalization.md).
type RowBuilder struct {
	names   map[string]string
	scratch []byte
	rich    bool
	hot     richHot             // скретч rich-маппинга (обнуляется finalize)
	norm    *sqlnorm.Normalizer // nil — нормализация SQL выключена
}

func NewRowBuilder(rich, sqlNorm bool) *RowBuilder {
	b := &RowBuilder{names: make(map[string]string, 128), rich: rich}
	if rich && sqlNorm {
		b.norm = &sqlnorm.Normalizer{}
	}
	return b
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
	if b.rich {
		return b.buildRich(f, datePrefix, filename, filePath)
	}
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

// buildRich — маппинг продуктовой схемы (семантика импортёра, см. rich.go):
// горячие свойства уходят в RichExt (первое вхождение), остальные — в
// Row.Props БЕЗ дедупликации (mapFilter импортёра сохраняет дубликаты).
func (b *RowBuilder) buildRich(f parser.EventFields, datePrefix, filename, filePath string) Row {
	r := Row{
		Time:     EventTime(datePrefix, f.TimePart), // bench-поле; rich ts — в Rich.Time
		Duration: ParseDuration(f.Duration),
		Event:    b.intern(f.Event),
		Level:    b.intern(f.Level),
		Filename: filename,
		FilePath: filePath,
		Rich:     &RichExt{},
	}
	r.bytes = rowFixedBytes + len(r.Event) + len(r.Level) + len(filename) + len(filePath)
	b.scratch = parser.ScanProps(f.Body, f.PropsAt, b.scratch, func(name, value []byte, _ bool) {
		isHot, keep := b.hot.dispatchHot(name, value)
		if isHot && !keep {
			r.bytes += len(value)
			return
		}
		key := b.intern(name)
		val := string(value)
		r.Props = append(r.Props, Pair{Name: key, Value: val})
		r.bytes += len(key) + len(val)
	})
	b.hot.finalize(r.Rich, datePrefix, f.TimePart, f.Duration, filePath, b.norm)
	// Сырые значения горячих свойств уже учтены при dispatchHot; добавляются
	// только производные поля и фиксированная часть.
	r.bytes += len(r.Rich.ContextLine) + 4*len(r.Rich.LockWaitConns) + richFixedBytes
	for i := range r.Rich.SQLParams {
		r.bytes += len(r.Rich.SQLParams[i]) + 8 // значения + оффсеты Array(String)
	}
	return r
}
