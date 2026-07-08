// Package follow — сценарий B bake-off (docs/bakeoff-protocol.md §1.1):
// режим --follow, слежение за растущим каталогом техжурнала с доставкой
// в ClickHouse и чекпоинтами «только после подтверждённой вставки».
//
// assemble.go — инкрементальная сборка событий из дозаписываемого файла.
// Семантика границ совпадает с батчевым parser.ScanEvents (маска начала
// строки ^\d{2}:\d{2}\.\d{6}-\d+, — format-spec §2.1) с той разницей, что
// конца файла в tail-режиме не существует. Правила закрытия события:
//
//  1. пришла следующая строка-маска → текущее событие завершено;
//  2. pending целиком оканчивается '\n' и нет новых данных idle-close-ms →
//     эмит по таймауту (idleEmit, вызывает владелец);
//  3. graceful-стоп → эмит \n-терминированной части pending (drain).
//
// Незавершённая последняя СТРОКА (без '\n') не эмитится никогда: и решение
// «маска / не маска» для строки принимается только когда строка завершена
// (для полной строки результат IsEventStart не зависит от байтов после её
// '\n' — скан маски не пересекает '\n').
package follow

import (
	"bytes"

	"tjagent/internal/parser"
)

// assembler — состояние сборки событий одного файла. Не потокобезопасен,
// принадлежит одному воркеру.
type assembler struct {
	pending []byte // неэмитированные байты файла начиная с baseOff
	baseOff int64  // абсолютный оффсет pending[0] в файле
	lineAt  int    // начало первой НЕзавершённой строки внутри pending
	inEvent bool   // pending начинается с завершённой строки-маски (идёт событие)
	bomDone bool   // решение о BOM принято (BOM пропускается только на оффсете 0, KI-6)
}

// readOff — абсолютный оффсет файла, с которого продолжать чтение.
func (a *assembler) readOff() int64 { return a.baseOff + int64(len(a.pending)) }

// reset — полный сброс на начало файла (усечение/пересоздание).
func (a *assembler) reset() {
	a.pending = a.pending[:0]
	a.baseOff = 0
	a.lineAt = 0
	a.inEvent = false
	a.bomDone = false
}

// resumeAt — продолжение с чекпоинта: off — оффсет первого непрочитанного
// байта. Чекпоинты пишутся только на границах строк, поэтому off всегда
// указывает на начало строки. BOM проверяется только при off == 0.
func (a *assembler) resumeAt(off int64) {
	a.reset()
	a.baseOff = off
	a.bomDone = off != 0
}

// cut отбрасывает первые n байт pending (они эмитированы или мусор до маски).
func (a *assembler) cut(n int) {
	if n == 0 {
		return
	}
	m := copy(a.pending, a.pending[n:])
	a.pending = a.pending[:m]
	a.baseOff += int64(n)
	if a.lineAt >= n {
		a.lineAt -= n
	} else {
		a.lineAt = 0
	}
}

// append скармливает свежепрочитанные байты и эмитит завершённые события.
// emit(ev, end): ev — байты события (включая завершающий '\n'), валидны
// только внутри вызова; end — абсолютный оффсет файла сразу за событием
// (кандидат в checkpoint после подтверждения вставки).
func (a *assembler) append(data []byte, emit func(ev []byte, end int64)) {
	a.pending = append(a.pending, data...)
	if !a.bomDone {
		if a.baseOff != 0 {
			a.bomDone = true
		} else {
			if len(a.pending) < 3 {
				return // ждём ещё байты (гейт MinFileSize гарантирует прогресс)
			}
			if a.pending[0] == 0xEF && a.pending[1] == 0xBB && a.pending[2] == 0xBF {
				a.cut(3)
			}
			a.bomDone = true
		}
	}
	a.scan(emit)
}

// scan продвигает разметку по завершённым строкам. Инвариант: при
// !inEvent lineAt == 0 (мусорные строки до первой маски отбрасываются
// целиком, как в батчевом сканере).
func (a *assembler) scan(emit func(ev []byte, end int64)) {
	for {
		idx := bytes.IndexByte(a.pending[a.lineAt:], '\n')
		if idx < 0 {
			return // незавершённая строка — ждём данных
		}
		next := a.lineAt + idx + 1
		if !a.inEvent {
			if parser.IsEventStart(a.pending) {
				a.inEvent = true
				a.lineAt = next
			} else {
				a.cut(next) // контент до первой строки-маски отбрасывается
			}
			continue
		}
		if a.lineAt > 0 && parser.IsEventStart(a.pending[a.lineAt:]) {
			// Правило 1: следующая строка-маска закрывает текущее событие
			n := a.lineAt
			emit(a.pending[:n], a.baseOff+int64(n))
			a.cut(n)
			a.lineAt = next - n // первая строка нового события уже завершена
		} else {
			a.lineAt = next // строка-продолжение текущего события
		}
	}
}

// idleEmit — правило 2: если идёт событие и pending ЦЕЛИКОМ оканчивается
// '\n' (нет незавершённой строки), эмитит его как завершённое. Вызывается
// владельцем при отсутствии новых данных дольше idle-close-ms.
// Возвращает true, если событие было эмитировано.
func (a *assembler) idleEmit(emit func(ev []byte, end int64)) bool {
	if !a.inEvent || len(a.pending) == 0 || a.lineAt != len(a.pending) {
		return false
	}
	emit(a.pending, a.baseOff+int64(len(a.pending)))
	a.cut(len(a.pending))
	a.inEvent = false
	return true
}

// drain — правило 3 (graceful-стоп): эмитит \n-терминированную часть
// текущего события; незавершённая последняя строка отбрасывается (её
// оффсет не чекпоинтится — перечитается при следующем запуске).
func (a *assembler) drain(emit func(ev []byte, end int64)) {
	if !a.inEvent || a.lineAt == 0 {
		return
	}
	emit(a.pending[:a.lineAt], a.baseOff+int64(a.lineAt))
	a.cut(a.lineAt)
	a.inEvent = false
}
