// fields.go — разбор события на поля БЕЗ JSON-сериализации.
//
// Точка подключения не-NDJSON приёмников (ClickHouse-sink): та же семантика
// разбора, что у AppendEvent/appendProps (единый контракт format-spec.md),
// но значения отдаются сырыми байтами — ровно тем текстом, который у
// NDJSON-пути оказался бы ВНУТРИ JSON-строки до экранирования (правила
// удвоенных кавычек уже применены: пара апострофов → апостроф, пара "" → ";
// многострочные значения сохраняют реальные \r\n). Зеркальность обоих путей
// закреплена дифференциальным
// тестом TestFieldsMirrorAppendEvent (fields_test.go): реконструкция NDJSON
// из полей обязана совпадать с AppendEvent байт-в-байт, включая golden-входы.
package parser

import "bytes"

// EventFields — заголовок события, разобранный по format-spec §2.2.
// Все срезы указывают в буфер события и валидны, пока валиден он сам
// (в потоковом конвейере — только до возврата из колбэка ScanEvents).
type EventFields struct {
	Body     []byte // событие после обрезки хвостовых \r\n (вход для ScanProps)
	TimePart []byte // "ММ:СС.мммммм" из маски
	Duration []byte // токен длительности: сырые цифры, без канонизации нулей
	Event    []byte // имя события
	Level    []byte // сырой токен уровня (типизация — забота приёмника)
	PropsAt  int    // смещение первого свойства в Body; len(Body), если свойств нет
}

// ParseEventFields — зеркало заголовочной части AppendEvent: обрезка
// хвостовых \r\n, разбор ММ:СС.мммммм-Длительность,Событие,Уровень[,...],
// правило «нет запятой после уровня → level съедает остаток события».
// ok=false ровно в тех случаях, когда AppendEvent отбрасывает событие
// (parse_skip, format-spec §6).
func ParseEventFields(ev []byte) (EventFields, bool) {
	end := len(ev)
	for end > 0 && (ev[end-1] == '\n' || ev[end-1] == '\r') {
		end--
	}
	ev = ev[:end]
	if len(ev) == 0 {
		return EventFields{}, false
	}

	comma := bytes.IndexByte(ev, ',')
	if comma < 0 {
		return EventFields{}, false
	}
	dash := bytes.IndexByte(ev[:comma], '-')
	if dash < 0 {
		return EventFields{}, false
	}
	f := EventFields{Body: ev, TimePart: ev[:dash], Duration: ev[dash+1 : comma]}

	p := comma + 1
	rel := bytes.IndexByte(ev[p:], ',')
	if rel < 0 {
		// Нет второй запятой после имени события → parse_skip (§6)
		return EventFields{}, false
	}
	f.Event = ev[p : p+rel]
	p += rel + 1

	if rel2 := bytes.IndexByte(ev[p:], ','); rel2 >= 0 {
		f.Level = ev[p : p+rel2]
		p += rel2 + 1
	} else {
		f.Level = ev[p:]
		p = len(ev)
	}
	f.PropsAt = p
	return f, true
}

// ScanProps — зеркало appendProps, отдающее сырые значения: проходит автомат
// свойств Имя=Значение (§3, §4) от смещения p (EventFields.PropsAt) и для
// каждой пары вызывает emit.
//
//   - name — имя свойства (сырые байты, всё до '=');
//   - value — значение как ТЕКСТ: для кавычечных значений — содержимое после
//     расклейки удвоенных кавычек (пара апострофов → апостроф, пара "" → "),
//     без JSON-экранирования; для бескавычечных — сырой токен (даже если
//     NDJSON типизировал бы его числом); пустое значение — пустой срез;
//   - quoted — значение было в кавычках (NDJSON в этом случае всегда строка;
//     иначе — типизация IsNumberToken + always-string, §4.2).
//
// scratch — переиспользуемый буфер для расклейки удвоенных кавычек;
// возвращается (возможно выросший) обратно. Срезы name/value валидны только
// внутри вызова emit: value может указывать как в ev, так и в scratch.
func ScanProps(ev []byte, p int, scratch []byte, emit func(name, value []byte, quoted bool)) []byte {
	end := len(ev)
	for p < end {
		eq := bytes.IndexByte(ev[p:end], '=')
		if eq < 0 {
			break // хвост без '=' молча отбрасывается (§3)
		}
		eqPos := p + eq
		name := ev[p:eqPos]

		p = eqPos + 1
		if p >= end {
			emit(name, nil, false) // Имя= в конце события → "" (§4.3)
			break
		}

		switch ev[p] {
		case '\'':
			// Одинарные кавычки: '' — экранирование; одиночная ' закрывает
			// значение только перед ',' или концом события (KI-10)
			p++
			valStart := p
			val := ev[p:p] // прямой подсрез ev, пока не потребовалась склейка
			joined := false
			seg := func(b []byte) {
				if joined {
					scratch = append(scratch, b...)
				} else {
					val = b
				}
			}
			lit := func() { // литеральная кавычка внутри данных
				if !joined {
					scratch = append(scratch[:0], val...)
					joined = true
				}
				scratch = append(scratch, '\'')
			}
			closed := false
			for p < end {
				idx := bytes.IndexByte(ev[p:end], '\'')
				if idx < 0 {
					seg(ev[valStart:end])
					p = end
					closed = true
					break
				}
				p += idx
				if p+1 < end && ev[p+1] == '\'' {
					// Экранирование '' → одна кавычка в данных
					seg(ev[valStart:p])
					lit()
					p += 2
					valStart = p
				} else if p+1 == end || ev[p+1] == ',' {
					// Закрывающая кавычка
					seg(ev[valStart:p])
					p++
					closed = true
					break
				} else {
					// Битый формат: одиночная ' внутри — считаем данными
					seg(ev[valStart:p])
					lit()
					p++
					valStart = p
				}
			}
			if !closed {
				seg(ev[valStart:p])
			}
			if joined {
				emit(name, scratch, true)
			} else {
				emit(name, val, true)
			}
		case '"':
			// Двойные кавычки: "" — экранирование; первая одиночная " закрывает безусловно
			p++
			valStart := p
			val := ev[p:p]
			joined := false
			seg := func(b []byte) {
				if joined {
					scratch = append(scratch, b...)
				} else {
					val = b
				}
			}
			lit := func() {
				if !joined {
					scratch = append(scratch[:0], val...)
					joined = true
				}
				scratch = append(scratch, '"')
			}
			closed := false
			for p < end {
				idx := bytes.IndexByte(ev[p:end], '"')
				if idx < 0 {
					seg(ev[valStart:end])
					p = end
					closed = true
					break
				}
				p += idx
				if p+1 < end && ev[p+1] == '"' {
					seg(ev[valStart:p])
					lit()
					p += 2
					valStart = p
					continue
				}
				seg(ev[valStart:p])
				p++
				closed = true
				break
			}
			if !closed {
				seg(ev[valStart:p])
			}
			if joined {
				emit(name, scratch, true)
			} else {
				emit(name, val, true)
			}
		default:
			// Без кавычек: до ',' или конца события
			sepPos := end
			if idx := bytes.IndexByte(ev[p:end], ','); idx >= 0 {
				sepPos = p + idx
			}
			emit(name, ev[p:sepPos], false)
			p = sepPos
		}

		if p < end && ev[p] == ',' {
			p++
		}
	}
	return scratch
}
