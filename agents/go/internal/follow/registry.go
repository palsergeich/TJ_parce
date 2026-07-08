// registry.go — реестр отслеживаемых файлов и продвижение чекпоинтов по
// ack'ам ClickHouse.
//
// Владение полями fileState разделено строго:
//   - path/id/worker — неизменяемы после регистрации;
//   - claimed/ident/committed/gen — под mu (пишут воркер и батчер-ack);
//   - readOff/size — атомики для прогресса/лага (пишут воркер и discovery).
//
// Продвижение committed корректно при усечениях: каждая строка несёт
// поколение файла (gen); resetGen() инкрементирует его, и ack устаревшего
// поколения игнорируется — оффсет старого содержимого не может «протолкнуть»
// чекпоинт нового.
package follow

import (
	"sync"
	"sync/atomic"

	"tjagent/internal/chsink"
)

// fileState — одна запись реестра.
type fileState struct {
	id     uint32 // индекс в registry.files (он же chsink.Src.File)
	path   string
	worker int

	mu        sync.Mutex
	claimed   bool     // идентичность установлена (первое открытие в этом запуске)
	ident     identity // валидно при claimed
	committed int64    // первый байт, НЕ подтверждённый ClickHouse
	gen       uint32   // поколение содержимого (усечение/пересоздание → +1)

	readOff atomic.Int64 // прочитано воркером (для лага)
	size    atomic.Int64 // последний размер от discovery (для лага)
}

// registry — реестр файлов + чекпоинты.
type registry struct {
	cp    *checkpoints
	mu    sync.RWMutex
	files []*fileState
	dirty atomic.Bool
}

// register добавляет файл (вызывает только discovery).
func (r *registry) register(path string, worker int) *fileState {
	r.mu.Lock()
	defer r.mu.Unlock()
	fs := &fileState{id: uint32(len(r.files)), path: path, worker: worker}
	r.files = append(r.files, fs)
	return fs
}

func (r *registry) file(id uint32) *fileState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.files[id]
}

// onAck — колбэк батчера chsink: строки batch'а подтверждены сервером.
// Единственный батчер шлёт батчи синхронно в порядке вставки, а строки
// одного файла производит один воркер в порядке файла ⇒ End-оффсеты каждого
// файла приходят монотонно; committed = max(committed, End) того же
// поколения — это и есть min-contiguous граница подтверждённого.
//
// Чекпоинт сохраняется сразу же (синхронно на горутине батчера): батчей —
// единицы в секунду, запись JSON — доли миллисекунды, зато окно дублей при
// kill -9 сжимается с периода сейвера до одного батча в полёте. Периодический
// сейвер остаётся страховкой на случай неудавшейся записи (dirty не снят).
func (r *registry) onAck(rows []chsink.Row) {
	r.mu.RLock()
	files := r.files
	r.mu.RUnlock()
	for i := range rows {
		src := rows[i].Src
		if int(src.File) >= len(files) {
			continue // строка не из follow-конвейера (не бывает; защита)
		}
		fs := files[src.File]
		fs.mu.Lock()
		if fs.gen == src.Gen && src.End > fs.committed {
			fs.committed = src.End
		}
		fs.mu.Unlock()
	}
	r.dirty.Store(true)
	r.saveIfDirty()
}

// claim — первое открытие файла в этом запуске: фиксирует идентичность и
// забирает committed_offset прошлого запуска (0 — файла в чекпоинтах нет,
// в т.ч. когда путь тот же, а идентичность другая — контрактный рестарт с 0).
func (r *registry) claim(fs *fileState, id identity) int64 {
	committed := r.cp.take(id)
	fs.mu.Lock()
	fs.claimed = true
	fs.ident = id
	fs.committed = committed
	fs.mu.Unlock()
	r.dirty.Store(true)
	return committed
}

// identSnapshot — текущая идентичность (для сверки при переоткрытии).
func (fs *fileState) identSnapshot() (identity, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.ident, fs.claimed
}

// genSnapshot — текущее поколение (метка строк).
func (fs *fileState) genSnapshot() uint32 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.gen
}

// resetGen — усечение файла: новое поколение, committed с нуля.
// Ack'и строк старого поколения после этого игнорируются.
func (r *registry) resetGen(fs *fileState) {
	fs.mu.Lock()
	fs.gen++
	fs.committed = 0
	fs.mu.Unlock()
	r.dirty.Store(true)
}

// rebind — файл пересоздан под тем же путём (идентичность сменилась):
// новое поколение, новая идентичность, committed с нуля.
func (r *registry) rebind(fs *fileState, id identity) {
	fs.mu.Lock()
	fs.gen++
	fs.ident = id
	fs.committed = 0
	fs.mu.Unlock()
	r.dirty.Store(true)
}

// snapshot — записи чекпоинтов всех привязанных файлов.
func (r *registry) snapshot() []checkpointRec {
	r.mu.RLock()
	files := r.files
	r.mu.RUnlock()
	recs := make([]checkpointRec, 0, len(files))
	for _, fs := range files {
		fs.mu.Lock()
		if fs.claimed {
			recs = append(recs, checkpointRec{Path: fs.path, Identity: fs.ident, Committed: fs.committed})
		}
		fs.mu.Unlock()
	}
	return recs
}

// saveIfDirty — периодическое сохранение чекпоинтов (сейвер).
func (r *registry) saveIfDirty() {
	if !r.dirty.Swap(false) {
		return
	}
	if err := r.cp.save(r.snapshot()); err != nil {
		r.dirty.Store(true) // не потеряли — попробуем в следующий тик
	}
}

// save — безусловное сохранение (финал).
func (r *registry) save() error {
	r.dirty.Store(false)
	return r.cp.save(r.snapshot())
}

// lag — число файлов и суммарное отставание чтения (наблюдаемый размер
// минус прочитанное) для прогресса/статистики.
func (r *registry) lag() (int, int64) {
	r.mu.RLock()
	files := r.files
	r.mu.RUnlock()
	var lag int64
	for _, fs := range files {
		if d := fs.size.Load() - fs.readOff.Load(); d > 0 {
			lag += d
		}
	}
	return len(files), lag
}
