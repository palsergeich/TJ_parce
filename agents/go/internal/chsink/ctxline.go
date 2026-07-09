// ctxline.go — «значимая строка контекста» (context_line) rich-схемы.
// Спека: docs/context-line.md.
//
// Контекст 1С — стек вызовов; в аналитике он живёт двумя колонками:
// context/context_hash (полный стек, группировка) и context_line («значимый
// отпечаток» — строка, по которой инженер узнаёт виновника).
//
// Стандартное правило (эквивалент импортёра import-jsonl.ps1) — последняя
// непустая строка Context (lastNonEmptyLine, chnum.go). Для операций СКД
// хвост стека — универсальная обвязка (ОбщийМодуль.КомпоновкаДанных.Модуль:
// ПроцессорВывода.Вывести(...) и т.п.), по которой НЕ видно, какой отчёт
// виноват. Умное правило СКД (опция context_skd_smart, по умолчанию
// включена): если значимая строка — вызов вывода результата компоновки
// (skdTriggers), виновник ищется ВЫШЕ по стеку — берётся самая глубокая
// строка, чей модуль принадлежит прикладному объекту (skdModulePrefixes);
// нет такой строки — фолбэк на стандартное правило.
//
// context и context_hash НЕ меняются никогда: хэш полного стека — ключ
// группировок (agg_context), непрерывность с загруженными данными обязана
// сохраняться при любом значении опции.
package chsink

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// skdTrigger — один триггерный метод: в коде значимой строки есть вызов
// «имя(» (имя + необязательные пробелы/табы + скобка). guards непуст —
// дополнительно строка обязана содержать хотя бы одну из подстрок: так
// универсальные имена (Инициализировать) остаются в границах правила СКД.
type skdTrigger struct {
	method string
	guards []string
}

// skdCompositionGuards — признаки СКД в строке для универсальных методов:
// «Компоновк» покрывает ПроцессорКомпоновкиДанных, МакетКомпоновки,
// КомпоновкаДанных...; DataComposition — английские конфигурации.
var skdCompositionGuards = []string{"Компоновк", "DataComposition"}

// skdTriggers — методы вывода результата СКД, v1 (русские и английские имена,
// как их пишет платформа; регистрозависимо). Список — данные, не код:
// расширение = добавление элемента (через решение владельца и правку
// docs/context-line.md).
var skdTriggers = []skdTrigger{
	{method: "Вывести"},
	{method: "ВывестиЭлемент"},
	{method: "НачатьВывод"},
	{method: "Инициализировать", guards: skdCompositionGuards},
	{method: "Output"},
	{method: "OutputItem"},
	{method: "BeginOutput"},
	{method: "Initialize", guards: skdCompositionGuards},
}

// skdModulePrefixes — префиксы модулей-«виновников»: прикладные объекты,
// которым принадлежит запуск отчёта. Точка в конце каждого префикса
// гарантирует совпадение ИМЕННО первого сегмента имени модуля
// («ОтчетНедели : …» не совпадает с «Отчет.»).
var skdModulePrefixes = []string{
	"Отчет.", "ВнешнийОтчет.", "Обработка.", "ВнешняяОбработка.",
	"Report.", "ExternalReport.", "DataProcessor.", "ExternalDataProcessor.",
}

// contextLine — context_line события: стандартное правило либо (при
// включённом context_skd_smart) правило СКД с фолбэком на стандартное.
// Форматные конвенции обеих ветвей одинаковы: сырая строка стека,
// все \r выброшены, пробелы/табы не обрезаются.
func contextLine(ctx string, skdSmart bool) string {
	last := lastNonEmptyLine(ctx)
	if !skdSmart || last == "" || !skdTriggered(last) {
		return last
	}
	if line, ok := deepestSKDCulprit(ctx); ok {
		return line
	}
	return last
}

// skdTriggered — есть ли в значимой строке вызов одного из триггерных методов.
// Проверяется вся строка «Модуль : строка : код»: имя модуля не содержит
// скобок, ложное срабатывание вне кода невозможно.
func skdTriggered(line string) bool {
	for i := range skdTriggers {
		t := &skdTriggers[i]
		if !containsCall(line, t.method) {
			continue
		}
		if len(t.guards) == 0 {
			return true
		}
		for _, g := range t.guards {
			if strings.Contains(line, g) {
				return true
			}
		}
	}
	return false
}

// containsCall — есть ли в s вызов name(...): вхождение name, слева от
// которого НЕ идентификаторный символ (буква/цифра/'_'; отсекает чужие
// методы с тем же хвостом — ПереВывести), а справа — необязательные
// пробелы/табы и '(' (отсекает идентификаторы с тем же началом —
// ВывестиВДокумент).
func containsCall(s, name string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], name)
		if i < 0 {
			return false
		}
		i += from
		from = i + 1
		if i > 0 {
			if r, _ := utf8.DecodeLastRuneInString(s[:i]); isIdentRune(r) {
				continue
			}
		}
		j := i + len(name)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j < len(s) && s[j] == '(' {
			return true
		}
	}
}

// isIdentRune — символ идентификатора 1С (буквы Unicode, цифры, '_').
func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// deepestSKDCulprit — самая глубокая (ближайшая к низу стека) строка
// контекста, чей модуль начинается с одного из skdModulePrefixes. Строки
// сканируются снизу вверх тем же разбиением, что lastNonEmptyLine (деление
// по '\n', все '\r' выбрасываются); ведущие пробелы/табы строки допускаются
// при сопоставлении, но возвращается строка целиком (без обрезки).
func deepestSKDCulprit(ctx string) (string, bool) {
	end := len(ctx)
	for end > 0 {
		start := strings.LastIndexByte(ctx[:end], '\n') + 1 // 0, если '\n' нет
		seg := ctx[start:end]
		if strings.IndexByte(seg, '\r') >= 0 {
			seg = strings.ReplaceAll(seg, "\r", "")
		}
		if hasSKDModulePrefix(seg) {
			return seg, true
		}
		end = start - 1 // пропустить сам '\n'
	}
	return "", false
}

// hasSKDModulePrefix — первый сегмент имени модуля строки стека начинается
// с одного из префиксов прикладных объектов (после ведущих пробелов/табов).
func hasSKDModulePrefix(line string) bool {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	rest := line[i:]
	for _, p := range skdModulePrefixes {
		if strings.HasPrefix(rest, p) {
			return true
		}
	}
	return false
}
