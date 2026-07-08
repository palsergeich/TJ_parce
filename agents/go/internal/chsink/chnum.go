// chnum.go — репликация числовых конверсий ClickHouse toXxxOrZero и строковых
// функций, которыми пользуется эталонный импортёр deploy/importer/import-jsonl.ps1.
//
// Семантика снята с живого сервера (24.8) батареей запросов и закреплена
// юнит-тестами (chnum_test.go):
//   - необязательный ведущий '+' (для знаковых — и '-'), затем ТОЛЬКО цифры;
//   - ведущие нули допустимы ('007' → 7);
//   - строка обязана быть исчерпана целиком ('5 ', '5abc', '1.5', '1e3' → 0);
//   - переполнение типа → 0 (НЕ насыщение: toUInt32OrZero('4294967296') = 0);
//   - пустая строка и одиночный знак → 0.
package chsink

import "strings"

// chUintOrZero — общий разбор беззнакового с верхней границей max.
func chUintOrZero(s string, max uint64) uint64 {
	if len(s) == 0 {
		return 0
	}
	i := 0
	if s[0] == '+' {
		i++
	}
	if i == len(s) {
		return 0
	}
	var v uint64
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		d := uint64(c - '0')
		if v > (max-d)/10 {
			return 0
		}
		v = v*10 + d
	}
	return v
}

func chUint64OrZero(s string) uint64 { return chUintOrZero(s, 1<<64-1) }
func chUint32OrZero(s string) uint32 { return uint32(chUintOrZero(s, 1<<32-1)) }
func chUint8OrZero(s string) uint8   { return uint8(chUintOrZero(s, 1<<8-1)) }

// chInt64OrZero — toInt64OrZero: знак '+'/'-', цифры, полная строка,
// переполнение int64 → 0 (минимум -9223372036854775808 валиден).
func chInt64OrZero(s string) int64 {
	if len(s) == 0 {
		return 0
	}
	neg := false
	i := 0
	switch s[0] {
	case '+':
		i++
	case '-':
		neg = true
		i++
	}
	if i == len(s) {
		return 0
	}
	limit := uint64(1) << 63 // |int64 min|
	if !neg {
		limit-- // int64 max
	}
	var v uint64
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		d := uint64(c - '0')
		if v > (limit-d)/10 {
			return 0
		}
		v = v*10 + d
	}
	if neg {
		return -int64(v) // корректно и для 1<<63
	}
	return int64(v)
}

// chTrimSpaces — trimBoth ClickHouse: срезает с обоих концов ТОЛЬКО пробелы
// (0x20); табуляция и прочие whitespace НЕ срезаются (проверено на сервере).
func chTrimSpaces(s string) string {
	b := 0
	e := len(s)
	for b < e && s[b] == ' ' {
		b++
	}
	for e > b && s[e-1] == ' ' {
		e--
	}
	return s[b:e]
}

// parseWaitConns — WaitConnections по формуле импортёра:
// if(v=”, [], arrayMap(x -> toUInt32OrZero(trimBoth(x)), splitByChar(',', v))).
// Пустые/нечисловые элементы дают 0 внутри массива ('7,9,' → [7,9,0]).
func parseWaitConns(v string) []uint32 {
	if v == "" {
		return nil
	}
	n := strings.Count(v, ",") + 1
	out := make([]uint32, 0, n)
	for {
		i := strings.IndexByte(v, ',')
		if i < 0 {
			out = append(out, chUint32OrZero(chTrimSpaces(v)))
			return out
		}
		out = append(out, chUint32OrZero(chTrimSpaces(v[:i])))
		v = v[i+1:]
	}
}

// lastNonEmptyLine — context_line импортёра:
// arrayLast(x -> x != ”, splitByChar('\n', replaceAll(ctx, '\r', ”))).
// Все '\r' выбрасываются (в том числе ВНУТРИ строки), деление по '\n',
// последняя непустая строка; нет непустых → "". Без обрезки пробелов/табов —
// импортёр их не трогает.
func lastNonEmptyLine(ctx string) string {
	end := len(ctx)
	for end > 0 {
		start := strings.LastIndexByte(ctx[:end], '\n') + 1 // 0, если '\n' нет
		seg := ctx[start:end]
		if strings.IndexByte(seg, '\r') >= 0 {
			seg = strings.ReplaceAll(seg, "\r", "")
		}
		if seg != "" {
			return seg
		}
		end = start - 1 // пропустить сам '\n'
		if end < 0 {
			break
		}
	}
	return ""
}

// firstPathSegment — collection импортёра:
// splitByChar('\\', replaceAll(file_path, '/', '\\'))[1] — первый сегмент
// пути при любом из двух разделителей; без разделителя — вся строка.
func firstPathSegment(fp string) string {
	if i := strings.IndexAny(fp, `\/`); i >= 0 {
		return fp[:i]
	}
	return fp
}

// CollectionOf — collection события по его file_path (первый сегмент
// относительного пути). Экспорт для follow-режима: лейбл collection метрик
// /metrics обязан совпадать с одноимённой колонкой rich-схемы.
func CollectionOf(filePath string) string { return firstPathSegment(filePath) }
