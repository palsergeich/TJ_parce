// checkpoint.go — persist-состояние follow-режима: по файлу {идентичность,
// committed_offset}. Оффсет продвигается ТОЛЬКО после подтверждения вставки
// ClickHouse (см. registry.onAck) — при краше возможны небольшие дубли
// (at-least-once), потери исключены.
//
// Идентичность файла — volume serial + file index (Windows,
// GetFileInformationByHandle) либо device+inode (POSIX). Путь в идентичность
// НЕ входит: 1С переиспользует имена YYMMDDHH.log при ротации. Файл
// чекпоинтов один на --state каталог, пишется атомарно (tmp + rename).
package follow

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// identity — идентичность файла, не зависящая от имени.
type identity struct {
	Vol uint64 `json:"vol"` // volume serial number (POSIX: dev)
	Hi  uint64 `json:"hi"`  // FileIndexHigh (POSIX: 0)
	Lo  uint64 `json:"lo"`  // FileIndexLow (POSIX: ino)
}

// checkpointRec — одна запись файла чекпоинтов.
type checkpointRec struct {
	Path      string   `json:"path"` // последний известный путь (диагностика; при резюме НЕ используется)
	Identity  identity `json:"identity"`
	Committed int64    `json:"committed_offset"` // первый байт, ещё не подтверждённый ClickHouse
}

// checkpointFile — формат <state>/checkpoints.json.
type checkpointFile struct {
	Version int             `json:"version"`
	Files   []checkpointRec `json:"files"`
}

const checkpointVersion = 1

// checkpoints — загруженные записи прошлых запусков + путь для сохранения.
// Записи, ещё не привязанные к живым файлам (claim), переживают сохранения:
// файл мог не вырасти/не открыться в этом запуске, его прогресс терять нельзя.
type checkpoints struct {
	path string

	mu     sync.Mutex
	loaded map[identity]checkpointRec
}

// loadCheckpoints читает состояние прошлого запуска; отсутствие или
// нечитаемость файла — чистый старт (с предупреждением во втором случае).
func loadCheckpoints(path string) *checkpoints {
	cp := &checkpoints{path: path, loaded: map[identity]checkpointRec{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[follow] чекпоинты %s не прочитаны (%v) — старт с нуля\n", path, err)
		}
		return cp
	}
	var f checkpointFile
	if err := json.Unmarshal(b, &f); err != nil {
		fmt.Fprintf(os.Stderr, "[follow] чекпоинты %s повреждены (%v) — старт с нуля\n", path, err)
		return cp
	}
	for _, r := range f.Files {
		cp.loaded[r.Identity] = r
	}
	return cp
}

// take забирает committed_offset прошлого запуска для идентичности id
// (0 — не было). Запись переходит во владение живого fileState и в
// снапшотах далее приходит из реестра.
func (cp *checkpoints) take(id identity) int64 {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	r, ok := cp.loaded[id]
	if !ok {
		return 0
	}
	delete(cp.loaded, id)
	return r.Committed
}

// leftovers — записи прошлых запусков, не привязанные к живым файлам
// (файл не рос/не открывался в этом запуске) — включаются в каждое сохранение.
func (cp *checkpoints) leftovers() []checkpointRec {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]checkpointRec, 0, len(cp.loaded))
	for _, r := range cp.loaded {
		out = append(out, r)
	}
	return out
}

// save пишет полный снапшот атомарно: tmp в том же каталоге + rename
// (os.Rename на Windows — MoveFileEx(REPLACE_EXISTING)).
func (cp *checkpoints) save(recs []checkpointRec) error {
	recs = append(recs, cp.leftovers()...)
	b, err := json.MarshalIndent(checkpointFile{Version: checkpointVersion, Files: recs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := cp.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cp.path)
}
