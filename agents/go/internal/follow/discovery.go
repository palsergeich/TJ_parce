// discovery.go — обнаружение файлов: рекурсивный обход --input раз в poll-ms.
//
// Ловит (a) рост известных файлов, (b) новые *.log (подхват < 2 с при
// poll-ms=500), (c) усечения (размер меньше прочитанного — решает воркер),
// (d) исчезновение/возврат пути (форсированный resend размера — ловит
// пересоздание файла с тем же размером). Гейт MinFileSize=100 перепроверяется
// каждый тик: файл короче 100 байт не регистрируется, пока не дорастёт.
package follow

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"tjagent/internal/parser"
)

type sizeMsg struct {
	id   uint32
	size int64
}

// dEntry — discovery-приватное состояние пути.
type dEntry struct {
	fs       *fileState
	sentSize int64  // последний размер, доставленный воркеру (-1 — форсировать)
	seen     uint64 // тик последнего появления пути в обходе
}

type discovery struct {
	reg        *registry
	workers    []*worker
	roots      []string // отслеживаемые каталоги (≥1; конфиг может дать список)
	entries    map[string]*dEntry
	tick       uint64
	nextWorker int
	walkWarned bool
}

// walk — один тик обнаружения: обход всех roots. Отправка размеров воркерам
// неблокирующая: полная очередь воркера (backpressure от ClickHouse) не
// останавливает обход и проверку stop-file — недоставленный размер
// повторится следующим тиком (sentSize не обновляется). Пересечение
// каталогов безопасно: entries ключуются полным путём (файл регистрируется
// однажды).
func (d *discovery) walk() {
	d.tick++
	for _, root := range d.roots {
		d.walkRoot(root)
	}
	// Исчезнувшие пути: при возврате форсируем resend (пересоздание файла
	// с тем же размером иначе осталось бы незамеченным).
	for _, e := range d.entries {
		if e.seen != d.tick {
			e.sentSize = -1
		}
	}
}

func (d *discovery) walkRoot(root string) {
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil // каталог мог исчезнуть между list и stat — не фатально
		}
		if !de.Type().IsRegular() {
			return nil
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".log") || strings.LastIndexByte(name, '.') == 0 {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		size := info.Size()
		e := d.entries[path]
		if e == nil {
			// Гейт MinFileSize: не регистрируем, пока файл не дорастёт до 100
			// байт — перепроверяется каждый тик по мере роста. Размер из
			// DirEntry — ленивые метаданные каталога NTFS: у файла, открытого
			// писателем, они занижены (вплоть до 0), поэтому перед пропуском
			// сверяемся точным os.Stat (он открывает файл и видит истину).
			if size < parser.MinFileSize {
				fi, err := os.Stat(path)
				if err != nil {
					return nil
				}
				size = fi.Size()
				if size < parser.MinFileSize {
					return nil
				}
			}
			fst := d.reg.register(path, d.nextWorker%len(d.workers))
			d.nextWorker++
			fst.size.Store(size)
			e = &dEntry{fs: fst, sentSize: -1}
			d.entries[path] = e
			debugf("[follow] новый файл: %s (%d Б) → воркер %d\n", path, size, fst.worker)
		}
		e.seen = d.tick
		// Дальше размер авторитетно отслеживает воркер (growthSweep по хэндлу —
		// кэш каталога для открытых писателем файлов заморожен); сообщение —
		// только пинок при изменении кэша (рост файлов, закрытых писателем,
		// оно ловит раньше дремлющего os.Stat-обхода).
		if size != e.sentSize {
			select {
			case d.workers[e.fs.worker].in <- sizeMsg{id: e.fs.id, size: size}:
				e.sentSize = size
			default: // очередь воркера полна — повтор следующим тиком
			}
		}
		return nil
	})
	if err != nil && !d.walkWarned {
		fmt.Fprintf(os.Stderr, "[follow] обход %s: %v\n", root, err)
		d.walkWarned = true
	}
}
