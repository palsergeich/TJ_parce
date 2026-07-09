// Package sqlnorm — нормализация текстов SQL техжурнала для корректной
// агрегации: значения-литералы ослепляются в '?', сами значения складываются
// в массив params (позиционно: i-й элемент массива = i-й '?' нормы), число
// параметров сохраняется отдельной колонкой. Единственный источник истины
// правил — docs/sql-normalization.md (спека v1, диалект MSSQL/DBMSSQL);
// реализация обязана воспроизводиться из спеки.
//
// Ключевой факт корпуса (см. спеку §2): 94% текстов DBMSSQL уже
// параметризованы — тело содержит '?', а значения приложены хвостом строк
// "p_N: значение". Нормализатор вырезает хвост целиком, а его значения
// подставляет в params по порядку '?' тела. Оставшиеся inline-литералы
// (0x…, числа, строки) ослепляются обычным порядком.
//
// Производительность: один проход по байтам без регэкспов и аллокаций на
// горячем пути; норм-текст пишется в переиспользуемый буфер Normalizer'а
// (валиден до следующего вызова — хэшировать сразу), элементы params в
// большинстве случаев алиасят входную строку (подстроки без копий).
package sqlnorm

// Dialect — метка диалекта правил. v1 реализует только MSSQL; текст с
// диалектом других СУБД (DBPOSTGRS и т.п.) нормализуется теми же правилами
// (документированная деградация, спека §7).
type Dialect uint8

const (
	// DialectMSSQL — правила v1 (docs/sql-normalization.md).
	DialectMSSQL Dialect = iota
)

// RulesVersion — версия набора правил (синхронизирована со спекой).
// Менять ТОЛЬКО вместе со спекой: смена версии = смена sql_norm_hash
// у части текстов = разрыв непрерывности групп в tj.events.
const RulesVersion = 1

// unresolved — значение params для '?' тела, которому не хватило значения
// хвоста p_N (в корпусе не встречается: равенство числа '?' тела и строк
// хвоста проверено на живых данных; спека §4.4).
const unresolved = "?"

// Normalizer — скретч одного потока-нормализатора (НЕ потокобезопасен).
// Буфер нормы переиспользуется между вызовами.
type Normalizer struct {
	buf   []byte // норм-текст (возвращается срезом, валиден до следующего вызова)
	qPos  []int  // индексы params, ожидающие значений хвоста p_N (по порядку '?')
	tails []string
}

// Normalize нормализует текст SQL по правилам v1 (MSSQL). Возвращает
// норм-текст (срез внутреннего буфера — валиден до следующего вызова;
// вызывающий обычно сразу считает cityHash64) и массив извлечённых значений
// (свежая аллокация, элементы могут алиасить sql).
func (n *Normalizer) Normalize(sql string) (norm []byte, params []string) {
	out := n.buf[:0]
	n.qPos = n.qPos[:0]
	n.tails = n.tails[:0]

	// Флаги списковых групп по глубине скобок (бит d — группа глубины d+1).
	// Глубже 64 уровней группы считаются несписковыми (коллапс не применяется).
	var listMask, valuesMask uint64
	depth := 0

	pendingKw := byte(0) // 0 — нет; 1 — IN; 2 — VALUES (ждёт следующую '(')
	valuesCont := false  // закрыта VALUES-группа: ",(" продолжает конструктор строк
	prevIdent := false   // предыдущий токен — идентификатор (цифры после — его часть)

	i := 0
	// Хвост p_N может начинаться с первого байта (вырожденный текст).
	if vs, ok := tailLine(sql, 0); ok {
		i = n.parseTail(sql, vs)
	}
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			// Схлопывание пробельной серии в один пробел; после каждого '\n' —
			// проверка начала хвоста "p_N: " (только с начала строки).
			for i < len(sql) {
				c = sql[i]
				if c == '\n' {
					if vs, ok := tailLine(sql, i+1); ok {
						i = n.parseTail(sql, vs)
						continue
					}
				} else if c != ' ' && c != '\t' && c != '\r' {
					break
				}
				i++
			}
			if len(out) > 0 && out[len(out)-1] != ' ' {
				out = append(out, ' ')
			}
			prevIdent = false // цифра после пробела — литерал, не хвост идентификатора
			continue          // pendingKw/valuesCont пробелы не сбрасывают

		case c == '\'':
			val, next := scanString(sql, i+1)
			params = append(params, val)
			out = n.emitPlaceholder(out, depth, listMask)
			i = next
			prevIdent, pendingKw, valuesCont = false, 0, false

		case isIdentStart(c):
			// N'...' — юникод-строка, если N не хвост идентификатора
			if (c == 'N' || c == 'n') && i+1 < len(sql) && sql[i+1] == '\'' {
				val, next := scanString(sql, i+2)
				params = append(params, val)
				out = n.emitPlaceholder(out, depth, listMask)
				i = next
				prevIdent, pendingKw, valuesCont = false, 0, false
				break
			}
			start := i
			for i < len(sql) && isIdentCont(sql[i]) {
				i++
			}
			out = append(out, sql[start:i]...)
			pendingKw = keyword(sql[start:i])
			prevIdent, valuesCont = true, false

		case c >= '0' && c <= '9':
			if prevIdent { // цифры внутри идентификатора (T1, _Fld123) не литералы
				out = append(out, c)
				i++
				break
			}
			val, next := scanNumber(sql, i)
			params = append(params, val)
			out = n.emitPlaceholder(out, depth, listMask)
			i = next
			prevIdent, pendingKw, valuesCont = false, 0, false

		case c == '.' && !prevIdent && i+1 < len(sql) && isDigit(sql[i+1]):
			val, next := scanNumber(sql, i)
			params = append(params, val)
			out = n.emitPlaceholder(out, depth, listMask)
			i = next
			prevIdent, pendingKw, valuesCont = false, 0, false

		case (c == '-' || c == '+') && unaryContext(out) && i+1 < len(sql) &&
			(isDigit(sql[i+1]) || (sql[i+1] == '.' && i+2 < len(sql) && isDigit(sql[i+2]))):
			// Однозначно унарный знак (после '(', ',', оператора или в начале) —
			// часть числового литерала, захватывается вместе со значением.
			val, next := scanNumber(sql, i)
			params = append(params, val)
			out = n.emitPlaceholder(out, depth, listMask)
			i = next
			prevIdent, pendingKw, valuesCont = false, 0, false

		case c == '?':
			// Плейсхолдер тела: значение придёт из хвоста p_N (резервируем слот).
			params = append(params, unresolved)
			n.qPos = append(n.qPos, len(params)-1)
			out = n.emitPlaceholder(out, depth, listMask)
			i++
			prevIdent, pendingKw, valuesCont = false, 0, false

		case c == '#':
			// Временная таблица: буквенный префикс сохраняется, с первой цифры
			// весь остаток имени отбрасывается (#tt123 → #tt). Не параметр.
			out = append(out, '#')
			i++
			for i < len(sql) && isIdentCont(sql[i]) && !isDigit(sql[i]) {
				out = append(out, sql[i])
				i++
			}
			for i < len(sql) && isIdentCont(sql[i]) {
				i++
			}
			prevIdent, pendingKw, valuesCont = true, 0, false

		case c == '@':
			// @P<n> → @P (структурное ослепление RPC-параметров); прочие
			// @-переменные копируются целиком (цифры — часть имени).
			out = append(out, '@')
			i++
			if i < len(sql) && (sql[i] == 'P' || sql[i] == 'p') && i+1 < len(sql) && isDigit(sql[i+1]) {
				out = append(out, sql[i])
				i++
				for i < len(sql) && isDigit(sql[i]) {
					i++
				}
				// хвост имени после цифр (не встречается) — копия как есть
			}
			for i < len(sql) && isIdentCont(sql[i]) {
				out = append(out, sql[i])
				i++
			}
			prevIdent, pendingKw, valuesCont = true, 0, false

		case c == '[':
			// [идентификатор] — копия как есть до ']' (']]' — экранирование)
			start := i
			i++
			for i < len(sql) {
				if sql[i] == ']' {
					if i+1 < len(sql) && sql[i+1] == ']' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			out = append(out, sql[start:i]...)
			prevIdent, pendingKw, valuesCont = true, 0, false

		case c == '(':
			depth++
			if depth <= 64 {
				bit := uint64(1) << (depth - 1)
				listMask &^= bit
				valuesMask &^= bit
				if pendingKw != 0 || valuesCont {
					listMask |= bit
					if pendingKw == 2 || valuesCont {
						valuesMask |= bit
					}
				}
			}
			out = append(out, '(')
			i++
			prevIdent, pendingKw, valuesCont = false, 0, false

		case c == ')':
			wasValues := false
			if depth >= 1 && depth <= 64 {
				wasValues = valuesMask&(uint64(1)<<(depth-1)) != 0
			}
			if depth > 0 {
				depth--
			}
			out = append(out, ')')
			i++
			prevIdent, pendingKw = false, 0
			if wasValues {
				out = collapseRowCtor(out)
				valuesCont = true // ",(" продолжит конструктор строк VALUES
			} else {
				valuesCont = false
			}

		case c == ',':
			out = append(out, ',')
			i++
			prevIdent, pendingKw = false, 0
			// valuesCont сохраняется: "),(" — продолжение конструктора строк

		default:
			out = append(out, c)
			i++
			prevIdent, pendingKw, valuesCont = false, 0, false
		}
	}
	if len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}

	// Значения хвоста p_N — в слоты '?' тела по порядку. Недостача → "?",
	// излишек отбрасывается (оба случая в корпусе отсутствуют, спека §4.4).
	for k, pos := range n.qPos {
		if k < len(n.tails) {
			params[pos] = n.tails[k]
		}
	}

	n.buf = out[:0]
	return out, params
}

// NormalizeString — удобная обёртка для тестов и отладки (копирует норму).
func (n *Normalizer) NormalizeString(sql string) (string, []string) {
	norm, params := n.Normalize(sql)
	return string(norm), params
}

// emitPlaceholder дописывает '?' с коллапсом списков: внутри группы,
// открытой после IN или VALUES, последовательность "?, ?" схлопывается в
// "?" (значения при этом уже захвачены в params — счётчик сохраняет
// истинную длину списка).
func (n *Normalizer) emitPlaceholder(out []byte, depth int, listMask uint64) []byte {
	if depth >= 1 && depth <= 64 && listMask&(uint64(1)<<(depth-1)) != 0 {
		j := len(out)
		if j > 0 && out[j-1] == ' ' {
			j--
		}
		if j > 0 && out[j-1] == ',' {
			j--
			if j > 0 && out[j-1] == ' ' {
				j--
			}
			if j > 0 && out[j-1] == '?' {
				return out[:j] // "?, ?" → "?"
			}
		}
	}
	return append(out, '?')
}

// collapseRowCtor схлопывает соседние конструкторы строк VALUES:
// суффикс "(?),(?)" (допустим одиночный пробел после запятой и перед
// скобкой) усечением до первого "(?)". Вызывается после ')' VALUES-группы.
func collapseRowCtor(out []byte) []byte {
	j := len(out)
	// ожидаемый суффикс справа налево: ( ? ) [sp] , [sp] ( ? )
	if j < 7 {
		return out
	}
	if out[j-1] != ')' || out[j-2] != '?' || out[j-3] != '(' {
		return out
	}
	k := j - 3
	if k > 0 && out[k-1] == ' ' {
		k--
	}
	if k == 0 || out[k-1] != ',' {
		return out
	}
	k--
	cut := k // усечение по запятую (включая её и пробелы вокруг)
	if k > 0 && out[k-1] == ' ' {
		k--
		cut = k
	}
	if k < 3 || out[k-1] != ')' || out[k-2] != '?' || out[k-3] != '(' {
		return out
	}
	return out[:cut]
}

// tailLine проверяет, начинается ли с pos строка хвоста параметров
// "p_<цифры>:[ ]" (правило действует только с начала строки текста).
// Возвращает позицию начала значения.
func tailLine(s string, pos int) (valStart int, ok bool) {
	i := pos
	if i+2 >= len(s) || s[i] != 'p' || s[i+1] != '_' {
		return 0, false
	}
	i += 2
	d := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == d || i >= len(s) || s[i] != ':' {
		return 0, false
	}
	i++
	if i < len(s) && s[i] == ' ' {
		i++
	}
	return i, true
}

// parseTail разбирает хвост значений "p_N: значение" (терминальный блок
// текста; значения-строки '...' могут содержать переводы строк — разбор
// кавычко-осознанный). Значения складываются в n.tails в порядке следования.
// Возвращает позицию продолжения (конец текста либо первая строка,
// не соответствующая формату хвоста, — тогда лексер продолжает с неё).
func (n *Normalizer) parseTail(s string, valStart int) int {
	i := valStart
	for {
		// значение текущей строки
		if i < len(s) && s[i] == '\'' {
			val, next := scanString(s, i+1)
			n.tails = append(n.tails, val)
			i = next
			// остаток строки после закрывающей кавычки игнорируется
			for i < len(s) && s[i] != '\n' {
				i++
			}
		} else {
			start := i
			for i < len(s) && s[i] != '\n' {
				i++
			}
			end := i
			if end > start && s[end-1] == '\r' {
				end--
			}
			n.tails = append(n.tails, s[start:end])
		}
		if i >= len(s) {
			return i
		}
		i++ // '\n'
		// пустые строки между значениями пропускаются
		for {
			j := i
			for j < len(s) && (s[j] == '\r' || s[j] == ' ' || s[j] == '\t') {
				j++
			}
			if j < len(s) && s[j] == '\n' {
				i = j + 1
				continue
			}
			break
		}
		if i >= len(s) {
			return i
		}
		vs, ok := tailLine(s, i)
		if !ok {
			return i // не хвост — вернуть управление лексеру (в корпусе не встречается)
		}
		i = vs
	}
}

// scanString сканирует строковый литерал с позиции ПОСЛЕ открывающего
// апострофа. Возвращает содержимое без кавычек ('' расклеено в ') и позицию
// за закрывающим апострофом. Незакрытая строка — до конца текста.
// Без экранирований значение алиасит вход (нулевое копирование).
func scanString(s string, start int) (val string, next int) {
	j := start
	esc := false
	for j < len(s) {
		if s[j] == '\'' {
			if j+1 < len(s) && s[j+1] == '\'' {
				esc = true
				j += 2
				continue
			}
			if !esc {
				return s[start:j], j + 1
			}
			return unescapeQuotes(s[start:j]), j + 1
		}
		j++
	}
	if !esc {
		return s[start:], j
	}
	return unescapeQuotes(s[start:]), j
}

func unescapeQuotes(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b = append(b, s[i])
		if s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'' {
			i++
		}
	}
	return string(b)
}

// scanNumber сканирует числовой литерал с позиции i (возможен ведущий знак,
// вызывающий уже проверил контекст): 0x… / 0X… — шестнадцатеричный;
// иначе цифры[.цифры][eE[±]цифры]; допускается ".5" и "5.". Возвращает
// значение как написано (алиас входа) и позицию за литералом.
func scanNumber(s string, i int) (val string, next int) {
	start := i
	if s[i] == '-' || s[i] == '+' {
		i++
	}
	if i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') {
		i += 2
		for i < len(s) && isHexDigit(s[i]) {
			i++
		}
		return s[start:i], i
	}
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	// экспонента: только если за e/E следует цифра (или знак и цифра) —
	// иначе e принадлежит следующему идентификатору
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			i = j
			for i < len(s) && isDigit(s[i]) {
				i++
			}
		}
	}
	return s[start:i], i
}

// unaryContext — знак перед цифрой однозначно унарный: норма пуста либо
// последний значащий байт нормы — открывающая скобка, запятая или оператор.
func unaryContext(out []byte) bool {
	j := len(out)
	if j > 0 && out[j-1] == ' ' {
		j--
	}
	if j == 0 {
		return true
	}
	switch out[j-1] {
	case '(', ',', '=', '<', '>', '+', '-', '*', '/', '%':
		return true
	}
	return false
}

// keyword распознаёт ключевые слова списковых групп (регистронезависимо):
// 1 — IN, 2 — VALUES, 0 — прочее.
func keyword(tok string) byte {
	switch len(tok) {
	case 2:
		if (tok[0]|0x20) == 'i' && (tok[1]|0x20) == 'n' {
			return 1
		}
	case 6:
		if (tok[0]|0x20) == 'v' && (tok[1]|0x20) == 'a' && (tok[2]|0x20) == 'l' &&
			(tok[3]|0x20) == 'u' && (tok[4]|0x20) == 'e' && (tok[5]|0x20) == 's' {
			return 2
		}
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// isIdentStart — начало идентификатора: латиница, '_', '$' и любые байты
// ≥ 0x80 (UTF-8-хвосты кириллических идентификаторов 1С).
func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' || c >= 0x80
}

// isIdentCont — продолжение идентификатора (плюс цифры, '#', '@').
func isIdentCont(c byte) bool {
	return isIdentStart(c) || isDigit(c) || c == '#' || c == '@'
}
