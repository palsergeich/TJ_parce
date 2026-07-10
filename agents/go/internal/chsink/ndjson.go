// ndjson.go — восстановление строки таблицы из NDJSON-строки события
// (формат docs/format-spec.md, вывод parser.AppendEvent). Точка реплея
// дискового буфера follow-режима (internal/wal): полезная нагрузка кадра —
// NDJSON, при дренаже она проходит через ТОТ ЖЕ маппинг и обогащение
// (bench: benchProp; rich: richProp + finalize со sqlnorm и context_line),
// что и прямой путь ParseEventFields → Build. Эквивалентность закреплена
// дифференциальным тестом TestBuildNDJSONMirrorsBuild (ndjson_test.go).
//
// Почему реконструкция без потерь возможна (гарантии format-spec):
//   - шесть заголовочных полей идут первыми в фиксированном порядке
//     (timestamp, duration, event, level, filename, file_path), всё после
//     них — свойства события в исходном порядке с сохранением дубликатов;
//   - значение JSON-строки после снятия экранирования — ровно тот текст,
//     который прямой путь получает из parser.ScanProps (расклейка удвоенных
//     кавычек уже применена при нормализации; зеркальность закреплена
//     TestFieldsMirrorAppendEvent в parser);
//   - бескавычечные числовые значения NDJSON хранит сырым токеном — прямой
//     путь передаёт те же байты;
//   - канонизация duration (KI-2: срез ведущих нулей) не меняет числового
//     значения ни в bench (ParseDuration), ни в rich (chUint64OrZero:
//     '007' → 7);
//   - timestamp длиной 26 распадается на datePrefix(14)+timePart(12);
//     длина 12 — деградированный файл (datePrefix ""), оба пути дают
//     эпоху 1970-01-01 при невалидной дате;
//   - collection выводится из file_path (CollectionOf == firstPathSegment)
//     идентично прямому пути — file_path в NDJSON есть.
//
// Никаких дополнительных метаданных кадру не требуется — NDJSON
// самодостаточен (проверка «minimal meta» из ADR).
//
// Декодер строгий: любое отклонение от формы, порождаемой AppendEvent
// (другой порядок заголовка, посторонние токены, битые escape), — ошибка.
// Кадр с валидным CRC, не прошедший декод, означает дрейф версий формата
// (буфер записан другой ревизией агента) или баг — владелец обязан жёстко
// остановиться, а не терять событие молча.
package chsink

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// BuildNDJSON строит строку таблицы из NDJSON-строки события (с завершающим
// '\n' или без). Схема строки (bench/rich, sqlnorm, context_skd_smart) — та,
// с которой создан RowBuilder. line обязана быть стабильной до возврата.
func (b *RowBuilder) BuildNDJSON(line []byte) (Row, error) {
	d := njDecoder{s: line}
	if err := d.expect('{'); err != nil {
		return Row{}, err
	}

	ts, err := d.headerString("timestamp")
	if err != nil {
		return Row{}, err
	}
	dur, err := d.headerNumber("duration")
	if err != nil {
		return Row{}, err
	}
	event, err := d.headerString("event")
	if err != nil {
		return Row{}, err
	}
	level, err := d.headerAny("level")
	if err != nil {
		return Row{}, err
	}
	filename, err := d.headerString("filename")
	if err != nil {
		return Row{}, err
	}
	filePath, err := d.headerString("file_path")
	if err != nil {
		return Row{}, err
	}

	var datePrefix string
	var timePart []byte
	switch len(ts) {
	case 26: // "20YY-MM-DDTHH:" + "ММ:СС.мммммм"
		datePrefix = string(ts[:14])
		timePart = ts[14:]
	case 12: // деградированное имя файла: только "ММ:СС.мммммм"
		timePart = ts
	default:
		return Row{}, fmt.Errorf("ndjson: timestamp длиной %d (ожидается 12 или 26)", len(ts))
	}

	r := Row{
		Time:     EventTime(datePrefix, timePart),
		Duration: ParseDuration(dur),
		Event:    b.intern(event),
		Level:    b.intern(level),
		Filename: string(filename),
		FilePath: string(filePath),
	}
	if b.rich {
		r.Rich = &RichExt{}
	}
	r.bytes = rowFixedBytes + len(r.Event) + len(r.Level) + len(r.Filename) + len(r.FilePath)

	// Свойства события: до '}' — пары "имя":значение в исходном порядке.
	for {
		c, err := d.next()
		if err != nil {
			return Row{}, err
		}
		if c == '}' {
			break
		}
		if c != ',' {
			return Row{}, fmt.Errorf("ndjson: ожидалась ',' или '}' на позиции %d, получено %q", d.p-1, c)
		}
		name, nb, err := d.str(d.nameBuf)
		if err != nil {
			return Row{}, err
		}
		d.nameBuf = nb
		if err := d.expect(':'); err != nil {
			return Row{}, err
		}
		value, err := d.value()
		if err != nil {
			return Row{}, err
		}
		// Обработчики копируют name/value до возврата — скретчи переиспользуемы.
		if b.rich {
			b.richProp(&r, name, value)
		} else {
			b.benchProp(&r, name, value)
		}
	}
	// Хвост: опциональный LF (полная NDJSON-строка) и ничего больше.
	if d.p < len(d.s) && d.s[d.p] == '\n' {
		d.p++
	}
	if d.p != len(d.s) {
		return Row{}, fmt.Errorf("ndjson: %d лишних байт после '}'", len(d.s)-d.p)
	}

	if b.rich {
		b.hot.finalize(r.Rich, datePrefix, timePart, dur, r.FilePath, b.norm, b.ctxSmart)
		// Зеркало buildRich: сырые значения горячих свойств учтены в richProp,
		// добавляются только производные поля и фиксированная часть.
		r.bytes += len(r.Rich.ContextLine) + 4*len(r.Rich.LockWaitConns) + richFixedBytes
		for i := range r.Rich.SQLParams {
			r.bytes += len(r.Rich.SQLParams[i]) + 8
		}
	}
	return r, nil
}

// njDecoder — строгий сканер NDJSON-строки события. Скретчи nameBuf/valBuf
// переиспользуются между свойствами (потребители копируют значения).
type njDecoder struct {
	s       []byte
	p       int
	nameBuf []byte
	valBuf  []byte
}

func (d *njDecoder) expect(c byte) error {
	if d.p >= len(d.s) || d.s[d.p] != c {
		return fmt.Errorf("ndjson: ожидался %q на позиции %d", c, d.p)
	}
	d.p++
	return nil
}

func (d *njDecoder) next() (byte, error) {
	if d.p >= len(d.s) {
		return 0, fmt.Errorf("ndjson: неожиданный конец на позиции %d", d.p)
	}
	c := d.s[d.p]
	d.p++
	return c, nil
}

// headerString — заголовочное поле-строка с фиксированным именем. Значение
// живёт до конца сборки: при наличии escape выделяется свежий буфер
// (без escape — подсрез line, стабильный по контракту BuildNDJSON).
func (d *njDecoder) headerString(name string) ([]byte, error) {
	if err := d.key(name); err != nil {
		return nil, err
	}
	v, _, err := d.str(nil)
	return v, err
}

// headerNumber — заголовочное поле-число (сырой токен).
func (d *njDecoder) headerNumber(name string) ([]byte, error) {
	if err := d.key(name); err != nil {
		return nil, err
	}
	return d.number()
}

// headerAny — заголовочное поле строка-или-число (level). Строковое значение
// декодируется с выделением (живёт до конца сборки), числовое — сырой токен.
func (d *njDecoder) headerAny(name string) ([]byte, error) {
	if err := d.key(name); err != nil {
		return nil, err
	}
	if d.p < len(d.s) && d.s[d.p] == '"' {
		v, _, err := d.str(nil)
		return v, err
	}
	return d.number()
}

// key — точное имя заголовочного поля: тот же порядок, что у AppendEvent;
// первое поле идёт сразу после '{', остальные — после ','.
func (d *njDecoder) key(name string) error {
	if d.p < len(d.s) && d.s[d.p] == ',' {
		d.p++
	}
	need := len(name) + 3 // "имя":
	if d.p+need > len(d.s) {
		return fmt.Errorf("ndjson: обрыв на заголовочном поле %q (позиция %d)", name, d.p)
	}
	if d.s[d.p] != '"' || string(d.s[d.p+1:d.p+1+len(name)]) != name ||
		d.s[d.p+1+len(name)] != '"' || d.s[d.p+2+len(name)] != ':' {
		return fmt.Errorf("ndjson: ожидалось заголовочное поле %q на позиции %d", name, d.p)
	}
	d.p += need
	return nil
}

// value — значение свойства: строка (декод в valBuf при escape) либо число
// (сырой токен). Иных типов AppendEvent не порождает.
func (d *njDecoder) value() ([]byte, error) {
	if d.p < len(d.s) && d.s[d.p] == '"' {
		v, sc, err := d.str(d.valBuf)
		d.valBuf = sc
		return v, err
	}
	return d.number()
}

// number — сырой числовой токен по грамматике RFC 8259 (продолжение до
// первого байта вне алфавита числа; строгую валидацию делал писатель).
func (d *njDecoder) number() ([]byte, error) {
	start := d.p
	for d.p < len(d.s) {
		c := d.s[d.p]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			d.p++
			continue
		}
		break
	}
	if d.p == start {
		return nil, fmt.Errorf("ndjson: ожидалось число на позиции %d", start)
	}
	return d.s[start:d.p], nil
}

// str — JSON-строка с текущей позиции (открывающая кавычка). Без escape —
// подсрез входа; с escape — декод в scratch (nil → свежий буфер). Возвращает
// значение и (возможно выросший) scratch.
func (d *njDecoder) str(scratch []byte) ([]byte, []byte, error) {
	if err := d.expect('"'); err != nil {
		return nil, scratch, err
	}
	start := d.p
	// Быстрый путь: до закрывающей кавычки без escape.
	for d.p < len(d.s) {
		c := d.s[d.p]
		if c == '"' {
			v := d.s[start:d.p]
			d.p++
			return v, scratch, nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return nil, scratch, fmt.Errorf("ndjson: сырой управляющий байт 0x%02x в строке (позиция %d)", c, d.p)
		}
		d.p++
	}
	// Медленный путь: расклейка escape в scratch.
	buf := append(scratch[:0], d.s[start:d.p]...)
	for d.p < len(d.s) {
		c := d.s[d.p]
		switch {
		case c == '"':
			d.p++
			return buf, buf, nil
		case c == '\\':
			d.p++
			if d.p >= len(d.s) {
				return nil, buf, fmt.Errorf("ndjson: обрыв escape на позиции %d", d.p)
			}
			e := d.s[d.p]
			d.p++
			switch e {
			case '"', '\\', '/':
				buf = append(buf, e)
			case 'b':
				buf = append(buf, '\b')
			case 'f':
				buf = append(buf, '\f')
			case 'n':
				buf = append(buf, '\n')
			case 'r':
				buf = append(buf, '\r')
			case 't':
				buf = append(buf, '\t')
			case 'u':
				r, err := d.hex4()
				if err != nil {
					return nil, buf, err
				}
				if utf16.IsSurrogate(rune(r)) {
					if d.p+1 < len(d.s) && d.s[d.p] == '\\' && d.s[d.p+1] == 'u' {
						d.p += 2
						r2, err := d.hex4()
						if err != nil {
							return nil, buf, err
						}
						dec := utf16.DecodeRune(rune(r), rune(r2))
						if dec == utf8.RuneError {
							return nil, buf, fmt.Errorf("ndjson: невалидная суррогатная пара на позиции %d", d.p)
						}
						buf = utf8.AppendRune(buf, dec)
					} else {
						return nil, buf, fmt.Errorf("ndjson: одиночный суррогат \\u%04x на позиции %d", r, d.p)
					}
				} else {
					buf = utf8.AppendRune(buf, rune(r))
				}
			default:
				return nil, buf, fmt.Errorf("ndjson: неизвестный escape \\%c на позиции %d", e, d.p-1)
			}
		case c < 0x20:
			return nil, buf, fmt.Errorf("ndjson: сырой управляющий байт 0x%02x в строке (позиция %d)", c, d.p)
		default:
			// Порция байт без escape целиком.
			q := d.p
			for q < len(d.s) && d.s[q] != '"' && d.s[q] != '\\' && d.s[q] >= 0x20 {
				q++
			}
			buf = append(buf, d.s[d.p:q]...)
			d.p = q
		}
	}
	return nil, buf, fmt.Errorf("ndjson: строка не закрыта (конец на позиции %d)", d.p)
}

func (d *njDecoder) hex4() (uint32, error) {
	if d.p+4 > len(d.s) {
		return 0, fmt.Errorf("ndjson: обрыв \\uXXXX на позиции %d", d.p)
	}
	var v uint32
	for i := 0; i < 4; i++ {
		c := d.s[d.p+i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		default:
			return 0, fmt.Errorf("ndjson: не hex-цифра в \\uXXXX на позиции %d", d.p+i)
		}
	}
	d.p += 4
	return v, nil
}
