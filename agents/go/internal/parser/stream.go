// stream.go — потоковый (чанковый) разрез файла на события.
//
// Зеркалит scan_file_windowed из core/src/pipeline.cpp: файл читается
// фиксированными чанками в переиспользуемый буфер, буфер всегда целиком
// содержит текущее событие [evStart, scan). Резидентность — O(ReadChunk +
// максимальное событие), а не O(размер файла), при байт-в-байт той же
// семантике, что у SplitEvents.
package parser

import (
	"bytes"
	"io"
)

const (
	// ReadChunk — целевой объём одного дочитывания буфера.
	ReadChunk = 8 << 20
	// GuardZone — гвардейская зона у конца буфера: строка-кандидат маски
	// события решается, только когда после её начала доступно ≥ GuardZone
	// байт либо конец файла (маске нужно ~15 байт + цифры длительности;
	// 64 КБ — с запасом, как kMapGuard в ядре).
	GuardZone = 64 << 10
)

// ScanEvents — потоковый эквивалент SplitEvents: читает r чанками в
// переиспользуемый буфер buf, режет содержимое на события по маске начала
// строки и вызывает emit для каждого. Семантика совпадает с SplitEvents:
// BOM в начале пропускается (KI-6), контент до первой строки-маски
// отбрасывается, чётность кавычек не проверяется (KI-1).
//
// Срез ev валиден только внутри вызова emit (буфер переиспользуется).
// Возвращает (возможно выросший) буфер для переиспользования вызывающим,
// число прочитанных байт и ошибку чтения (io.EOF ошибкой не считается;
// уже выданные события при ошибке остаются выданными — как в ядре).
func ScanEvents(r io.Reader, buf []byte, emit func(ev []byte)) ([]byte, uint64, error) {
	return scanEvents(r, buf, ReadChunk, GuardZone, emit)
}

// scanEvents — реализация с параметрами чанка/гварда (тестируется на малых
// значениях, чтобы прогнать переезды буфера на коротких входах).
func scanEvents(r io.Reader, buf []byte, readChunk, guard int, emit func(ev []byte)) ([]byte, uint64, error) {
	if cap(buf) < readChunk+guard {
		buf = make([]byte, 0, readChunk+guard)
	}
	buf = buf[:0]
	var bytesRead uint64
	atEOF := false

	// refill дочитывает буфер до полной ёмкости либо до конца файла.
	refill := func() error {
		for !atEOF && len(buf) < cap(buf) {
			n, err := r.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
			bytesRead += uint64(n)
			if err == io.EOF {
				atEOF = true
				return nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	}

	if err := refill(); err != nil {
		return buf, bytesRead, err
	}

	scan, evStart := 0, 0
	if len(buf) >= 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		scan, evStart = 3, 3
	}
	inEvent := IsEventStart(buf[scan:])

	// advance двигает окно вперёд, не теряя начала текущего события:
	// байты до evStart выбрасываются, буфер дочитывается. Если событие
	// занимает весь буфер, буфер растёт — прогресс гарантирован
	// (аналог переезда/роста окна в scan_file_windowed).
	advance := func() error {
		if evStart > 0 {
			n := copy(buf, buf[evStart:])
			buf = buf[:n]
			scan -= evStart
			evStart = 0
		}
		if cap(buf)-len(buf) < readChunk {
			newCap := 2 * cap(buf)
			if min := len(buf) + readChunk + guard; newCap < min {
				newCap = min
			}
			nb := make([]byte, len(buf), newCap)
			copy(nb, buf)
			buf = nb
		}
		return refill()
	}

	for {
		// Маска решается только в безопасной зоне: до конца буфера остаётся
		// ≥ guard байт (или конец файла) — кандидат у края чанка не решается
		// по обрезанному префиксу строки.
		safeEnd := len(buf)
		if !atEOF {
			safeEnd -= guard
		}
		if scan >= safeEnd {
			if atEOF {
				break
			}
			if err := advance(); err != nil {
				return buf, bytesRead, err
			}
			continue
		}
		idx := bytes.IndexByte(buf[scan:safeEnd], '\n')
		if idx < 0 {
			scan = safeEnd
			if atEOF {
				break
			}
			if err := advance(); err != nil {
				return buf, bytesRead, err
			}
			continue
		}
		nextLine := scan + idx + 1
		scan = nextLine
		if nextLine < len(buf) && IsEventStart(buf[nextLine:]) {
			if inEvent {
				emit(buf[evStart:nextLine])
			}
			inEvent = true
			evStart = nextLine
		}
	}
	if inEvent && len(buf) > evStart {
		emit(buf[evStart:])
	}
	return buf, bytesRead, nil
}
