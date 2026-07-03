// Package parser — нормализация технологического журнала 1С в NDJSON.
//
// Порт семантики cpp_parse/count_contexts.cpp байт-в-байт по спецификации
// docs/format-spec.md v1.0 (ревизия 2). Любое отклонение от спеки — баг:
// golden-суита (tests/golden/run_golden.ps1) сравнивает вывод побайтно
// с эталоном C++-нормализатора.
package parser

import "bytes"

// MinFileSize — файлы короче пропускаются целиком (format-spec §6).
const MinFileSize = 100

// DateFromFilename разбирает имя файла YYMMDDHH.log в префикс "20YY-MM-DDTHH:".
// Первые 8 символов обязаны быть цифрами, суффикс и диапазоны не проверяются
// (format-spec §3, поле timestamp). Иначе — пустая строка (timestamp деградирует
// до MM:SS.ssssss, файл считается аномалией).
func DateFromFilename(name string) string {
	if len(name) < 8 {
		return ""
	}
	for i := 0; i < 8; i++ {
		if name[i] < '0' || name[i] > '9' {
			return ""
		}
	}
	return "20" + name[0:2] + "-" + name[2:4] + "-" + name[4:6] + "T" + name[6:8] + ":"
}

// IsEventStart — маска начала события: ^\d{2}:\d{2}\.\d{6}-\d+, (format-spec §2.1).
// b — срез от начала физической строки до конца данных (маска может «смотреть»
// за пределы строки, но \n там не пройдёт проверку «цифра или запятая»).
func IsEventStart(b []byte) bool {
	if len(b) < 15 {
		return false
	}
	if !(isDigit(b[0]) && isDigit(b[1]) && b[2] == ':' &&
		isDigit(b[3]) && isDigit(b[4]) && b[5] == '.' &&
		isDigit(b[6]) && isDigit(b[7]) && isDigit(b[8]) &&
		isDigit(b[9]) && isDigit(b[10]) && isDigit(b[11]) &&
		b[12] == '-') {
		return false
	}
	hasDigits := false
	for i := 13; i < len(b); i++ {
		c := b[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigits = true
		case c == ',':
			return hasDigits
		default:
			return false
		}
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// IsNumberToken — строгая грамматика JSON-числа RFC 8259, длина ≤ 32
// (format-spec §4.2, KI-2): -?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?
func IsNumberToken(v []byte) bool {
	if len(v) == 0 || len(v) > 32 {
		return false
	}
	i := 0
	if v[i] == '-' {
		i++
		if i == len(v) {
			return false
		}
	}
	// Целая часть: 0 или [1-9][0-9]*
	switch {
	case v[i] == '0':
		i++
	case v[i] >= '1' && v[i] <= '9':
		for i < len(v) && isDigit(v[i]) {
			i++
		}
	default:
		return false
	}
	// Дробная часть
	if i < len(v) && v[i] == '.' {
		i++
		if i == len(v) || !isDigit(v[i]) {
			return false
		}
		for i < len(v) && isDigit(v[i]) {
			i++
		}
	}
	// Экспонента
	if i < len(v) && (v[i] == 'e' || v[i] == 'E') {
		i++
		if i < len(v) && (v[i] == '+' || v[i] == '-') {
			i++
		}
		if i == len(v) || !isDigit(v[i]) {
			return false
		}
		for i < len(v) && isDigit(v[i]) {
			i++
		}
	}
	return i == len(v)
}

// isAlwaysStringField — поля, которые никогда не типизируются числом (format-spec §4.2).
func isAlwaysStringField(name []byte) bool {
	// string([]byte) в сравнении не аллоцирует (оптимизация компилятора Go)
	s := string(name)
	return s == "SearchString" || s == "Guid" || s == "UUID"
}

const hexDigits = "0123456789abcdef"

// AppendEscaped дописывает s в dst с JSON-экранированием (format-spec §4.4):
// `"`, `\`, \b \f \n \r \t, прочие < 0x20 → \u00xx (hex в нижнем регистре).
// Байты ≥ 0x20 копируются как есть, UTF-8 не валидируется (KI-3).
func AppendEscaped(dst []byte, s []byte) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if i > start {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return dst
}

// SplitEvents режет содержимое файла на события по маске начала строки
// (format-spec §2.1) и вызывает emit для каждого. BOM в начале файла
// пропускается (KI-6). Контент до первой строки-маски отбрасывается.
// Чётность кавычек НЕ проверяется — KI-1 воспроизводится сознательно
// (golden-кейс mask_inside_quotes остаётся XFAIL до починки в core).
func SplitEvents(data []byte, emit func(ev []byte)) {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	n := len(data)
	ptr := 0
	eventStart := 0
	inEvent := IsEventStart(data)
	for ptr < n {
		idx := bytes.IndexByte(data[ptr:], '\n')
		if idx < 0 {
			break
		}
		ptr += idx + 1
		if ptr < n && IsEventStart(data[ptr:]) {
			if inEvent {
				emit(data[eventStart:ptr])
			}
			inEvent = true
			eventStart = ptr
		}
	}
	if inEvent && n-eventStart > 0 {
		emit(data[eventStart:n])
	}
}

// AppendEvent разбирает одно событие и дописывает в dst готовую JSON-строку
// с завершающим '\n'. Возвращает (dst, false), если событие отбрасывается
// (нет второй запятой в заголовке и т.п. — parse_skip, format-spec §6).
//
// datePrefix — "20YY-MM-DDTHH:" или ""; filenameEsc/filePathEsc — уже
// JSON-экранированные значения полей filename/file_path (общие на файл).
func AppendEvent(dst []byte, ev []byte, datePrefix string, filenameEsc, filePathEsc []byte) ([]byte, bool) {
	// Хвостовые \r\n события обрезаются (внутренние сохраняются), §2.1
	end := len(ev)
	for end > 0 && (ev[end-1] == '\n' || ev[end-1] == '\r') {
		end--
	}
	ev = ev[:end]
	if len(ev) == 0 {
		return dst, false
	}

	// Заголовок: ММ:СС.мммммм-Длительность,Событие,Уровень[,...] (§2.2)
	comma := bytes.IndexByte(ev, ',')
	if comma < 0 {
		return dst, false
	}
	dash := bytes.IndexByte(ev[:comma], '-')
	if dash < 0 {
		return dst, false
	}
	timePart := ev[:dash]
	duration := ev[dash+1 : comma]
	// Канонизация duration: без ведущих нулей, "000" → "0" (KI-2)
	for len(duration) > 1 && duration[0] == '0' {
		duration = duration[1:]
	}

	p := comma + 1
	rel := bytes.IndexByte(ev[p:], ',')
	if rel < 0 {
		// Нет второй запятой после имени события → parse_skip (§6)
		return dst, false
	}
	eventName := ev[p : p+rel]
	p += rel + 1

	// Уровень — до следующей запятой; если её нет, level съедает весь остаток
	// события и свойства не разбираются (поведение эталона, см. golden short_header)
	var level []byte
	if rel2 := bytes.IndexByte(ev[p:], ','); rel2 >= 0 {
		level = ev[p : p+rel2]
		p += rel2 + 1
	} else {
		level = ev[p:]
		p = len(ev)
	}

	dst = append(dst, `{"timestamp":"`...)
	dst = append(dst, datePrefix...)
	dst = append(dst, timePart...) // маска гарантирует только цифры/':'/'.'
	dst = append(dst, `","duration":`...)
	dst = append(dst, duration...)
	dst = append(dst, `,"event":"`...)
	dst = AppendEscaped(dst, eventName)
	dst = append(dst, `","level":`...)
	if IsNumberToken(level) {
		dst = append(dst, level...)
	} else {
		dst = append(dst, '"')
		dst = AppendEscaped(dst, level)
		dst = append(dst, '"')
	}
	dst = append(dst, `,"filename":"`...)
	dst = append(dst, filenameEsc...)
	dst = append(dst, `","file_path":"`...)
	dst = append(dst, filePathEsc...)
	dst = append(dst, '"')

	// Свойства Имя=Значение (§3, §4)
	dst = appendProps(dst, ev, p)

	dst = append(dst, '}', '\n')
	return dst, true
}

// appendProps — автомат свойств: имя до '=', значение по правилам кавычек §4.1
// либо без кавычек до ',' с типизацией §4.2. Хвост без '=' молча отбрасывается.
func appendProps(dst []byte, ev []byte, p int) []byte {
	end := len(ev)
	for p < end {
		eq := bytes.IndexByte(ev[p:end], '=')
		if eq < 0 {
			break
		}
		eqPos := p + eq
		name := ev[p:eqPos]

		dst = append(dst, ',', '"')
		dst = AppendEscaped(dst, name)
		dst = append(dst, '"', ':')

		p = eqPos + 1
		if p >= end {
			dst = append(dst, '"', '"')
			break
		}

		switch ev[p] {
		case '\'':
			// Одинарные кавычки: '' — экранирование; одиночная ' закрывает
			// значение только перед ',' или концом события (KI-10)
			dst = append(dst, '"')
			p++
			valStart := p
			closed := false
			for p < end {
				idx := bytes.IndexByte(ev[p:end], '\'')
				if idx < 0 {
					dst = AppendEscaped(dst, ev[valStart:end])
					dst = append(dst, '"')
					p = end
					closed = true
					break
				}
				p += idx
				if p+1 < end && ev[p+1] == '\'' {
					// Экранирование '' → одна кавычка в данных
					dst = AppendEscaped(dst, ev[valStart:p])
					dst = append(dst, '\'')
					p += 2
					valStart = p
				} else if p+1 == end || ev[p+1] == ',' {
					// Закрывающая кавычка
					dst = AppendEscaped(dst, ev[valStart:p])
					dst = append(dst, '"')
					p++
					closed = true
					break
				} else {
					// Битый формат: одиночная ' внутри — считаем данными
					dst = AppendEscaped(dst, ev[valStart:p])
					dst = append(dst, '\'')
					p++
					valStart = p
				}
			}
			if !closed {
				dst = AppendEscaped(dst, ev[valStart:p])
				dst = append(dst, '"')
			}
		case '"':
			// Двойные кавычки: "" — экранирование; первая одиночная " закрывает безусловно
			dst = append(dst, '"')
			p++
			valStart := p
			closed := false
			for p < end {
				idx := bytes.IndexByte(ev[p:end], '"')
				if idx < 0 {
					dst = AppendEscaped(dst, ev[valStart:end])
					dst = append(dst, '"')
					p = end
					closed = true
					break
				}
				p += idx
				if p+1 < end && ev[p+1] == '"' {
					dst = AppendEscaped(dst, ev[valStart:p])
					dst = append(dst, '\\', '"')
					p += 2
					valStart = p
					continue
				}
				dst = AppendEscaped(dst, ev[valStart:p])
				dst = append(dst, '"')
				p++
				closed = true
				break
			}
			if !closed {
				dst = AppendEscaped(dst, ev[valStart:p])
				dst = append(dst, '"')
			}
		default:
			// Без кавычек: до ',' или конца события; число по строгой грамматике,
			// кроме always-string полей
			sepPos := end
			if idx := bytes.IndexByte(ev[p:end], ','); idx >= 0 {
				sepPos = p + idx
			}
			val := ev[p:sepPos]
			if !isAlwaysStringField(name) && IsNumberToken(val) {
				dst = append(dst, val...)
			} else {
				dst = append(dst, '"')
				dst = AppendEscaped(dst, val)
				dst = append(dst, '"')
			}
			p = sepPos
		}

		if p < end && ev[p] == ',' {
			p++
		}
	}
	return dst
}
