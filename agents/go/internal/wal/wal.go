// Package wal — дисковый буфер follow-режима (WAL) по референс-дизайну Vector
// (docs/adr-vector.md §1, принят 2026-07-09).
//
// Роль в конвейере: tail-чтение → разбор → [WAL] → отправитель → ClickHouse.
// Точка ack смещается: чекпоинт ТЖ-файла двигается после durable-записи
// события в буфер (fsync), а НЕ после подтверждения ClickHouse — агент
// переживает долгий простой БД без роста памяти. Внутренний курсор читателя
// (cursor.json в каталоге буфера) двигается только после ack вставки;
// полностью подтверждённые сегменты удаляются.
//
// Формат на диске:
//
//	<dir>/seg-<seq>.wal — append-only сегменты по ~SegmentBytes (128 МиБ);
//	кадр = uint32 LE длина + uint32 LE CRC-32C (Castagnoli) полезной нагрузки
//	+ полезная нагрузка (NDJSON-строка события формата docs/format-spec.md).
//	CRC-32C выбран из-за аппаратного ускорения (SSE4.2) в stdlib Go.
//	<dir>/cursor.json — позиция читателя {seg, off}, атомарно (tmp+rename).
//
// Долговечность: групповой fsync раз в FsyncEvery (≤500 мс) и при ротации/
// закрытии; per-frame fsync нет. Колбэк OnDurable отдаёт метаданные кадров
// СТРОГО после успешного fsync — это точка продвижения чекпоинтов ТЖ.
//
// Лимит размера (MaxBytes) соблюдается жёстко: when_full = block —
// Append блокируется до освобождения места (удаления подтверждённых
// сегментов); ТЖ-файлы служат внешним WAL, чекпоинты просто перестают
// двигаться. При блокировке непустой текущий сегмент принудительно
// ротируется, чтобы стать удаляемым после подтверждения его событий
// (иначе кап меньше сегмента давал бы вечную блокировку).
//
// Повреждения: при открытии последний сегмент сканируется по кадрам
// (длина+CRC), хвост усечён до последнего валидного кадра с логом потерь;
// более ранние сегменты проверяются CRC лениво при чтении — несовпадение
// там означает порчу уже сброшенного на диск (битый носитель) и трактуется
// как фатальная ошибка. Любая ошибка I/O буфера (запись/fsync/чтение) —
// фатал: Fatal() закрывается, все операции возвращают ошибку; владелец
// обязан ЖЁСТКО остановить агент (принцип Vector — не терять молча).
//
// Правило сайзинга (дословно из документации Vector): «оценивайте размер
// события по его JSON-представлению без сжатия».
package wal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tjagent/internal/metrics"
)

const (
	// DefaultSegmentBytes — целевой размер сегмента (ADR: ~128 МиБ).
	DefaultSegmentBytes = 128 << 20
	// DefaultFsyncEvery — период группового fsync (ADR: ≤500 мс).
	DefaultFsyncEvery = 500 * time.Millisecond
	// MaxFrameBytes — вменяемый предел кадра (максимум наблюдался TLOCK
	// ~3.4 МБ сырых байт; NDJSON крупнее за счёт экранирования — запас ×16).
	// Большее значение длины при чтении считается порчей.
	MaxFrameBytes = 64 << 20

	frameHeader = 8 // uint32 длина + uint32 CRC-32C

	segPrefix = "seg-"
	segSuffix = ".wal"
	cursorFn  = "cursor.json"

	// readChunk — гранула упреждающего чтения читателя.
	readChunk = 512 << 10
)

// castagnoli — таблица CRC-32C (аппаратное ускорение в stdlib).
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrStopped — Append после остановки приёма (graceful-стоп): событие не
// записано, его чекпоинт не двигался — при следующем запуске перечитается.
var ErrStopped = errors.New("wal: приём остановлен")

// Meta — происхождение кадра для продвижения чекпоинтов ТЖ после fsync
// (зеркало chsink.Src без импорта: id файла в реестре follow, поколение
// содержимого, оффсет файла сразу за событием).
type Meta struct {
	File uint32
	Gen  uint32
	End  int64
}

// Pos — позиция в буфере: сегмент + байтовый оффсет внутри него.
// Позиции кадров строго монотонны в порядке записи.
type Pos struct {
	Seg uint64 `json:"seg"`
	Off int64  `json:"off"`
}

// Before — строгий порядок позиций.
func (p Pos) Before(q Pos) bool {
	return p.Seg < q.Seg || (p.Seg == q.Seg && p.Off < q.Off)
}

// segment — один файл буфера.
type segment struct {
	seq     uint64
	path    string
	size    int64    // записано байт (валидная длина)
	durable int64    // fsync'нуто байт (читателю доступно ≤ durable)
	closed  bool     // ротация завершена: size финален, файл закрыт
	f       *os.File // хэндл писателя (nil у закрытых)
}

// ageMark — граница fsync-группы для gauge tj_buffer_oldest_unacked_seconds:
// «все кадры до pos стали durable не позже t». Метки прореживаются до ~1/с.
type ageMark struct {
	pos Pos
	t   time.Time
}

// Config — параметры буфера.
type Config struct {
	// Dir — каталог сегментов (создаётся). Обязателен.
	Dir string
	// MaxBytes — жёсткий предел суммарного размера живых сегментов.
	// Валидация продуктового минимума (256 МиБ) — забота вызывающего
	// (agentcfg); сам пакет принимает любой положительный (юнит-тесты).
	MaxBytes int64
	// SegmentBytes — целевой размер сегмента (0 → DefaultSegmentBytes).
	SegmentBytes int64
	// FsyncEvery — период группового fsync (0 → DefaultFsyncEvery).
	FsyncEvery time.Duration
	// OnDurable вызывается после каждого успешного fsync с метаданными
	// покрытых кадров в порядке записи (точка продвижения чекпоинтов ТЖ).
	// Вызов идёт под внутренним мьютексом буфера — колбэк не должен
	// обращаться к WAL и обязан быть быстрым (запись checkpoints.json — мс).
	OnDurable func([]Meta)
	// Logf — операционный журнал (nil — молча). Формат printf, без '\n'.
	Logf func(format string, a ...any)
}

// WAL — дисковый буфер. Все методы потокобезопасны. Писатели (Append) —
// любые горутины; читатель (TryNext/WaitData) — ровно одна горутина;
// Ack — горутина батчера (по одному вызову на подтверждённый батч).
type WAL struct {
	cfg Config

	mu    sync.Mutex
	space *sync.Cond // освобождение места / стоп / фатал

	segs    []*segment // по возрастанию seq; открытым может быть только последний
	nextSeq uint64
	total   int64  // Σ size живых сегментов (точное соблюдение MaxBytes)
	pending []Meta // метаданные записанных, но ещё не fsync'нутых кадров

	cursor  Pos // подтверждено ClickHouse (персистентно, cursor.json)
	readPos Pos // прочитано читателем (память; readPos ≥ cursor)

	// Читатель: собственный хэндл и буфер упреждающего чтения.
	rd      *os.File
	rdSeq   uint64
	rbuf    []byte // окно файла [rbufOff, rbufOff+len(rbuf))
	rbufOff int64

	ages []ageMark

	stopping bool // graceful-стоп: блокированные Append'ы отпускаются с ErrStopped
	closed   bool // приём закрыт окончательно (после финального fsync)

	fatalErr  error
	fatalCh   chan struct{}
	fatalOnce sync.Once

	dataCh   chan struct{} // пинок читателю: durable вырос / сегмент создан
	syncDone chan struct{} // syncer остановлен
	syncStop chan struct{}
}

// Open создаёт/открывает буфер: восстановление последнего сегмента
// (усечение битого хвоста), загрузка курсора, уборка полностью
// подтверждённых сегментов, дозапись в последний живой сегмент.
func Open(cfg Config) (*WAL, error) {
	if cfg.Dir == "" {
		return nil, errors.New("wal: пустой каталог буфера")
	}
	if cfg.MaxBytes <= 0 {
		return nil, errors.New("wal: MaxBytes должен быть положительным")
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = DefaultSegmentBytes
	}
	if cfg.FsyncEvery <= 0 {
		cfg.FsyncEvery = DefaultFsyncEvery
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: создание каталога %s: %w", cfg.Dir, err)
	}

	w := &WAL{
		cfg:      cfg,
		fatalCh:  make(chan struct{}),
		dataCh:   make(chan struct{}, 1),
		syncDone: make(chan struct{}),
		syncStop: make(chan struct{}),
	}
	w.space = sync.NewCond(&w.mu)

	if err := w.recover(); err != nil {
		return nil, err
	}

	go w.syncer()
	return w, nil
}

// recover — восстановление состояния каталога при открытии. Порядок важен:
// уборка orphan-сегментов → скан/усечение последнего → зажим курсора →
// уборка полностью подтверждённого последнего → переоткрытие живого хвоста
// на дозапись.
func (w *WAL) recover() error {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return fmt.Errorf("wal: чтение каталога %s: %w", w.cfg.Dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, segPrefix) || !strings.HasSuffix(name, segSuffix) {
			continue
		}
		seqStr := strings.TrimSuffix(strings.TrimPrefix(name, segPrefix), segSuffix)
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			w.cfg.Logf("[wal] посторонний файл в буфере игнорируется: %s", name)
			continue
		}
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("wal: атрибуты %s: %w", name, err)
		}
		w.segs = append(w.segs, &segment{
			seq:    seq,
			path:   filepath.Join(w.cfg.Dir, name),
			size:   info.Size(),
			closed: true,
		})
	}
	sort.Slice(w.segs, func(i, j int) bool { return w.segs[i].seq < w.segs[j].seq })
	w.nextSeq = 1
	if n := len(w.segs); n > 0 {
		w.nextSeq = w.segs[n-1].seq + 1
	}

	// Курсор читателя (может отсутствовать — чистый старт или новый буфер).
	if len(w.segs) > 0 {
		w.cursor = Pos{Seg: w.segs[0].seq}
	} else {
		w.cursor = Pos{Seg: w.nextSeq}
	}
	if b, err := os.ReadFile(filepath.Join(w.cfg.Dir, cursorFn)); err == nil {
		var c Pos
		if err := json.Unmarshal(b, &c); err != nil {
			w.cfg.Logf("[wal] cursor.json повреждён (%v) — реплей с начала буфера (дубли возможны, потерь нет)", err)
		} else {
			w.cursor = c
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("wal: чтение cursor.json: %w", err)
	}

	// Уборка сегментов, полностью подтверждённых до краша (окно
	// «cursor сохранён, файл не удалён»).
	kept := w.segs[:0]
	for _, s := range w.segs {
		if s.seq < w.cursor.Seg {
			if err := os.Remove(s.path); err != nil {
				return fmt.Errorf("wal: удаление подтверждённого сегмента %s: %w", s.path, err)
			}
			w.cfg.Logf("[wal] удалён полностью подтверждённый сегмент %s (уборка после рестарта)", filepath.Base(s.path))
			continue
		}
		kept = append(kept, s)
	}
	w.segs = kept

	// Восстановление последнего сегмента: скан кадров, усечение битого хвоста.
	if n := len(w.segs); n > 0 {
		last := w.segs[n-1]
		valid, dropped, err := scanValid(last.path)
		if err != nil {
			return err
		}
		if dropped > 0 {
			w.cfg.Logf("[wal] %s: битый хвост %d Б после оффсета %d усечён до последнего валидного кадра. События хвоста, не покрытые чекпоинтами ТЖ (обычный краш при записи), перечитаются из журналов; покрытые (порча уже fsync'нутых данных носителем) — потеряны вместе с испорченными байтами",
				filepath.Base(last.path), dropped, valid)
			if err := os.Truncate(last.path, valid); err != nil {
				return fmt.Errorf("wal: усечение %s: %w", last.path, err)
			}
			last.size = valid
		}
	}

	// Нормализация курсора против усечения/пропусков.
	w.clampCursorLocked()

	// Полностью подтверждённый последний сегмент (курсор на его конце)
	// убирается до переоткрытия: реплей не нужен, дозапись начнётся с нового.
	if s := w.segBySeq(w.cursor.Seg); s != nil && w.cursor.Off >= s.size {
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("wal: удаление подтверждённого сегмента %s: %w", s.path, err)
		}
		w.cfg.Logf("[wal] удалён полностью подтверждённый сегмент %s (уборка после рестарта)", filepath.Base(s.path))
		kept := w.segs[:0]
		for _, x := range w.segs {
			if x.seq != s.seq {
				kept = append(kept, x)
			}
		}
		w.segs = kept
		w.cursor = Pos{Seg: s.seq + 1}
	}

	// Переоткрытие живого хвоста на дозапись (непрерывность нумерации),
	// если последний сегмент не дорос до цели; иначе он остаётся закрытым.
	if n := len(w.segs); n > 0 {
		last := w.segs[n-1]
		if last.size < w.cfg.SegmentBytes {
			f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return fmt.Errorf("wal: открытие %s для дозаписи: %w", last.path, err)
			}
			// Персистим усечение/состояние до начала новых записей.
			if err := f.Sync(); err != nil {
				_ = f.Close()
				return fmt.Errorf("wal: fsync %s: %w", last.path, err)
			}
			last.f = f
			last.closed = false
		}
	}
	for _, s := range w.segs {
		s.durable = s.size // существовавшее при старте содержимое считается durable
		w.total += s.size
	}

	w.readPos = w.cursor

	// Затравка оценки возраста старейшего неподтверждённого: mtime сегмента
	// курсора — время ПОСЛЕДНЕЙ записи в него, т.е. нижняя граница возраста
	// (реальный старейший кадр старше). Уточняется по мере новых fsync.
	if s := w.segBySeq(w.cursor.Seg); s != nil && w.backlogLocked() {
		t := time.Now()
		if fi, err := os.Stat(s.path); err == nil {
			t = fi.ModTime()
		}
		w.ages = append(w.ages, ageMark{pos: w.durableEndLocked(), t: t})
	}
	return nil
}

// clampCursorLocked приводит курсор к существующим данным: сегмент курсора
// удалён/отсутствует → первый существующий с большим seq; оффсет за концом
// усечённого сегмента → конец (подтверждённые кадры уже в БД — не потеря).
func (w *WAL) clampCursorLocked() {
	if len(w.segs) == 0 {
		if w.cursor.Seg < w.nextSeq {
			w.cursor = Pos{Seg: w.nextSeq}
		}
		w.cursor.Off = 0
		return
	}
	s := w.segBySeq(w.cursor.Seg)
	if s == nil {
		for _, cand := range w.segs {
			if cand.seq > w.cursor.Seg {
				w.cursor = Pos{Seg: cand.seq}
				return
			}
		}
		w.cursor = Pos{Seg: w.nextSeq}
		return
	}
	if w.cursor.Off > s.size {
		w.cfg.Logf("[wal] курсор (%d,%d) за концом сегмента %d Б — зажат (усечение хвоста затронуло подтверждённые кадры; они уже в БД)",
			w.cursor.Seg, w.cursor.Off, s.size)
		w.cursor.Off = s.size
	}
}

// scanValid — скан кадров сегмента с проверкой длины и CRC; возвращает
// длину валидного префикса и число отброшенных байт хвоста.
func scanValid(path string) (valid, dropped int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("wal: открытие %s для восстановления: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("wal: атрибуты %s: %w", path, err)
	}
	size := fi.Size()

	var off int64
	hdr := make([]byte, frameHeader)
	var payload []byte
	for off+frameHeader <= size {
		if _, err := f.ReadAt(hdr, off); err != nil {
			return 0, 0, fmt.Errorf("wal: чтение заголовка кадра %s@%d: %w", path, off, err)
		}
		length := int64(binary.LittleEndian.Uint32(hdr[0:4]))
		crc := binary.LittleEndian.Uint32(hdr[4:8])
		if length == 0 || length > MaxFrameBytes || off+frameHeader+length > size {
			break // мусорная длина либо недописанный кадр — хвост усекается
		}
		if int64(cap(payload)) < length {
			payload = make([]byte, length)
		}
		p := payload[:length]
		if _, err := f.ReadAt(p, off+frameHeader); err != nil {
			return 0, 0, fmt.Errorf("wal: чтение кадра %s@%d: %w", path, off, err)
		}
		if crc32.Checksum(p, castagnoli) != crc {
			break // CRC не сошёлся — кадр и всё после него отбрасываются
		}
		off += frameHeader + length
	}
	return off, size - off, nil
}

func (w *WAL) segBySeq(seq uint64) *segment {
	// Сегментов единицы (MaxBytes/SegmentBytes ≈ 8) — линейный поиск.
	for _, s := range w.segs {
		if s.seq == seq {
			return s
		}
	}
	return nil
}

// backlogLocked — есть ли durable-кадры, не подтверждённые БД.
func (w *WAL) backlogLocked() bool {
	end := w.durableEndLocked()
	return w.cursor.Before(end)
}

// durableEndLocked — позиция сразу за последним durable-байтом.
func (w *WAL) durableEndLocked() Pos {
	if len(w.segs) == 0 {
		return Pos{Seg: w.nextSeq}
	}
	last := w.segs[len(w.segs)-1]
	return Pos{Seg: last.seq, Off: last.durable}
}

// setFatalIO — фатальная ошибка I/O буфера (запись/fsync/чтение/курсор):
// инкремент tj_buffer_write_errors_total + общий фатал.
func (w *WAL) setFatalIO(err error) {
	metrics.BufferWriteError()
	w.setFatal(err)
}

func (w *WAL) setFatal(err error) {
	w.fatalOnce.Do(func() {
		w.fatalErr = err
		close(w.fatalCh)
		w.cfg.Logf("[wal] ФАТАЛЬНАЯ ОШИБКА БУФЕРА: %v", err)
	})
	w.space.Broadcast()
	w.notifyData()
}

// Fail помечает буфер фатально неисправным извне (например, кадр с валидным
// CRC не декодировался — дрейф версий формата NDJSON). Fatal() закрывается,
// владелец обязан жёстко остановить агент.
func (w *WAL) Fail(err error) { w.setFatal(err) }

// Fatal закрывается при фатальной ошибке I/O буфера. Владелец обязан
// жёстко остановить агент (ненулевой exit) — продолжать работу нельзя.
func (w *WAL) Fatal() <-chan struct{} { return w.fatalCh }

// FatalErr — первая фатальная ошибка (после закрытия Fatal()).
func (w *WAL) FatalErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fatalErr
}

func (w *WAL) fatalNow() bool {
	select {
	case <-w.fatalCh:
		return true
	default:
		return false
	}
}

func (w *WAL) notifyData() {
	select {
	case w.dataCh <- struct{}{}:
	default:
	}
}

// Append дописывает кадр с полезной нагрузкой payload и метаданными meta.
// Блокируется, когда буфер полон (when_full = block), до освобождения места
// либо начала остановки (ErrStopped). Ошибка I/O — фатальна (Fatal()).
// payload копируется — срез можно переиспользовать сразу после возврата.
func (w *WAL) Append(payload []byte, meta Meta) error {
	need := int64(len(payload)) + frameHeader
	if len(payload) == 0 || int64(len(payload)) > MaxFrameBytes {
		err := fmt.Errorf("wal: недопустимый размер кадра %d Б (максимум %d)", len(payload), MaxFrameBytes)
		w.setFatal(err)
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for {
		if w.fatalErr != nil {
			return w.fatalErr
		}
		if w.closed || w.stopping {
			return ErrStopped
		}
		if w.total+need <= w.cfg.MaxBytes {
			break
		}
		// Полный буфер: непустой открытый сегмент ротируется, чтобы после
		// подтверждения его событий стать удаляемым (кап < сегмента иначе
		// блокировал бы навсегда), затем ожидание освобождения места.
		if cur := w.curLocked(); cur != nil && cur.size > 0 {
			if err := w.rotateLocked(); err != nil {
				return err
			}
			continue
		}
		w.space.Wait()
	}

	cur := w.curLocked()
	if cur != nil && cur.size+need > w.cfg.SegmentBytes {
		if err := w.rotateLocked(); err != nil {
			return err
		}
		cur = nil
	}
	if cur == nil {
		var err error
		cur, err = w.openSegLocked()
		if err != nil {
			return err
		}
	}

	var hdr [frameHeader]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, castagnoli))
	if _, err := cur.f.Write(hdr[:]); err != nil {
		err = fmt.Errorf("wal: запись заголовка кадра в %s: %w", cur.path, err)
		w.setFatalIO(err)
		return err
	}
	if _, err := cur.f.Write(payload); err != nil {
		err = fmt.Errorf("wal: запись кадра в %s: %w", cur.path, err)
		w.setFatalIO(err)
		return err
	}
	cur.size += need
	w.total += need
	w.pending = append(w.pending, meta)
	return nil
}

// curLocked — открытый (последний) сегмент либо nil.
func (w *WAL) curLocked() *segment {
	if n := len(w.segs); n > 0 && !w.segs[n-1].closed {
		return w.segs[n-1]
	}
	return nil
}

// openSegLocked — создание следующего сегмента.
func (w *WAL) openSegLocked() (*segment, error) {
	path := filepath.Join(w.cfg.Dir, fmt.Sprintf("%s%012d%s", segPrefix, w.nextSeq, segSuffix))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		err = fmt.Errorf("wal: создание сегмента %s: %w", path, err)
		w.setFatalIO(err)
		return nil, err
	}
	s := &segment{seq: w.nextSeq, path: path, f: f}
	w.nextSeq++
	w.segs = append(w.segs, s)
	return s, nil
}

// rotateLocked — закрытие текущего сегмента: fsync, доставка pending-метаданных
// (они durable), закрытие хэндла. Следующий сегмент создаётся лениво.
func (w *WAL) rotateLocked() error {
	cur := w.curLocked()
	if cur == nil {
		return nil
	}
	if err := w.syncLocked(cur); err != nil {
		return err
	}
	if err := cur.f.Close(); err != nil {
		err = fmt.Errorf("wal: закрытие сегмента %s: %w", cur.path, err)
		w.setFatalIO(err)
		return err
	}
	cur.f = nil
	cur.closed = true
	w.notifyData()
	return nil
}

// syncLocked — fsync сегмента + продвижение durable + доставка OnDurable +
// метка возраста. Единственная точка, после которой чекпоинты ТЖ имеют
// право двигаться за записанные кадры.
func (w *WAL) syncLocked(s *segment) error {
	if s.durable == s.size {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		err = fmt.Errorf("wal: fsync %s: %w", s.path, err)
		w.setFatalIO(err)
		return err
	}
	s.durable = s.size
	w.addAgeMarkLocked(Pos{Seg: s.seq, Off: s.durable})
	if len(w.pending) > 0 {
		batch := w.pending
		w.pending = nil
		if w.cfg.OnDurable != nil {
			w.cfg.OnDurable(batch)
		}
	}
	w.notifyData()
	return nil
}

// addAgeMarkLocked — метка «всё до pos durable к now»; прореживание до ~1/с
// (замена последней метки: её кадры получают более раннее время — возраст
// слегка завышается, что консервативно для алерта).
func (w *WAL) addAgeMarkLocked(pos Pos) {
	now := time.Now()
	if n := len(w.ages); n > 0 && now.Sub(w.ages[n-1].t) < time.Second {
		w.ages[n-1].pos = pos
		return
	}
	w.ages = append(w.ages, ageMark{pos: pos, t: now})
}

// syncer — групповой fsync по таймеру FsyncEvery.
func (w *WAL) syncer() {
	defer close(w.syncDone)
	t := time.NewTicker(w.cfg.FsyncEvery)
	defer t.Stop()
	for {
		select {
		case <-w.syncStop:
			return
		case <-w.fatalCh:
			return
		case <-t.C:
			w.mu.Lock()
			if cur := w.curLocked(); cur != nil {
				_ = w.syncLocked(cur) // ошибка уже зафиксирована setFatal
			}
			w.mu.Unlock()
		}
	}
}

// BeginShutdown переводит буфер в режим остановки: Append'ы, блокированные
// на полном буфере (и все последующие ПОЛНЫЕ ожидания), отпускаются с
// ErrStopped; обычные записи продолжают приниматься до CloseWriter.
// Вызывать ДО остановки воркеров — иначе заблокированный на полном буфере
// воркер не увидит сигнал стопа.
func (w *WAL) BeginShutdown() {
	w.mu.Lock()
	w.stopping = true
	w.mu.Unlock()
	w.space.Broadcast()
}

// CloseWriter останавливает приём окончательно: финальный fsync текущего
// сегмента (доставка последних OnDurable) — события, успевшие в буфер,
// durable. Вызывать после остановки всех писателей.
func (w *WAL) CloseWriter() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.stopping = true
	close(w.syncStop)
	if cur := w.curLocked(); cur != nil {
		if err := w.syncLocked(cur); err != nil {
			return err
		}
		if err := cur.f.Close(); err != nil {
			err = fmt.Errorf("wal: закрытие сегмента %s: %w", cur.path, err)
			w.setFatalIO(err)
			return err
		}
		cur.f = nil
		cur.closed = true
	}
	// Ставший закрытым полностью подтверждённый сегмент убирается сразу:
	// после полного дренажа стоп оставляет пустой каталог буфера.
	if w.fatalErr == nil {
		if err := w.reapLocked(); err != nil {
			return err
		}
	}
	w.space.Broadcast()
	w.notifyData()
	return nil
}

// Close — финальное закрытие (после остановки читателя): хэндлы, курсор.
func (w *WAL) Close() {
	_ = w.CloseWriter()
	<-w.syncDone
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rd != nil {
		_ = w.rd.Close()
		w.rd = nil
	}
	// Курсор уже персистится на каждом Ack; финальная запись — страховка.
	if w.fatalErr == nil {
		_ = w.persistCursorLocked()
	}
}

// ---------------------------------------------------------------- читатель ---

// TryNext возвращает следующий durable-кадр либо ok=false, когда читатель
// догнал durable-границу. Полезная нагрузка валидна до следующего вызова
// TryNext. Ошибка — фатальная порча/I/O (агент обязан остановиться).
func (w *WAL) TryNext() (payload []byte, pos Pos, ok bool, err error) {
	for {
		w.mu.Lock()
		if w.fatalErr != nil {
			err := w.fatalErr
			w.mu.Unlock()
			return nil, Pos{}, false, err
		}
		s := w.segBySeq(w.readPos.Seg)
		if s == nil {
			// Сегмент удалён (Ack) или ещё не создан: вперёд к первому
			// существующему с большим seq, иначе данных нет.
			moved := false
			for _, cand := range w.segs {
				if cand.seq > w.readPos.Seg {
					w.readPos = Pos{Seg: cand.seq}
					moved = true
					break
				}
			}
			if !moved {
				w.mu.Unlock()
				return nil, Pos{}, false, nil
			}
			w.mu.Unlock()
			continue
		}
		limit := s.durable
		if s.closed {
			limit = s.size
		}
		if w.readPos.Off >= limit {
			if s.closed {
				// Сегмент дочитан; следующий появится у писателя (лениво).
				next := w.segByAfter(s.seq)
				if next == nil {
					w.mu.Unlock()
					return nil, Pos{}, false, nil
				}
				w.readPos = Pos{Seg: next.seq}
				w.mu.Unlock()
				continue
			}
			w.mu.Unlock()
			return nil, Pos{}, false, nil // открытый сегмент: ждём fsync
		}
		seq, path, off := s.seq, s.path, w.readPos.Off
		w.mu.Unlock()

		p, newOff, err := w.readFrame(seq, path, off, limit)
		if err != nil {
			w.setFatalIO(err)
			return nil, Pos{}, false, err
		}
		newPos := Pos{Seg: seq, Off: newOff}
		w.mu.Lock()
		w.readPos = newPos
		w.mu.Unlock()
		return p, newPos, true, nil
	}
}

func (w *WAL) segByAfter(seq uint64) *segment {
	for _, s := range w.segs {
		if s.seq > seq {
			return s
		}
	}
	return nil
}

// readFrame — чтение кадра по (seq, off) с упреждающим окном и проверкой CRC.
// Вызывается только горутиной читателя (rd/rbuf принадлежат ей).
func (w *WAL) readFrame(seq uint64, path string, off, limit int64) ([]byte, int64, error) {
	if w.rd == nil || w.rdSeq != seq {
		if w.rd != nil {
			_ = w.rd.Close()
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, fmt.Errorf("wal: открытие сегмента для чтения %s: %w", path, err)
		}
		w.rd = f
		w.rdSeq = seq
		w.rbuf = w.rbuf[:0]
		w.rbufOff = off
	}
	ensure := func(n int64) error {
		if off >= w.rbufOff && off+n <= w.rbufOff+int64(len(w.rbuf)) {
			return nil
		}
		want := n
		if want < readChunk {
			want = readChunk
		}
		if max := limit - off; want > max {
			want = max
		}
		if int64(cap(w.rbuf)) < want {
			w.rbuf = make([]byte, want)
		}
		w.rbuf = w.rbuf[:want]
		if _, err := w.rd.ReadAt(w.rbuf, off); err != nil {
			return fmt.Errorf("wal: чтение %s@%d (%d Б): %w", path, off, want, err)
		}
		w.rbufOff = off
		return nil
	}

	if err := ensure(frameHeader); err != nil {
		return nil, 0, err
	}
	h := w.rbuf[off-w.rbufOff:]
	length := int64(binary.LittleEndian.Uint32(h[0:4]))
	crc := binary.LittleEndian.Uint32(h[4:8])
	if length == 0 || length > MaxFrameBytes || off+frameHeader+length > limit {
		return nil, 0, fmt.Errorf("wal: порча сегмента %s@%d: длина кадра %d при durable-границе %d — буфер повреждён вне восстановимого хвоста",
			path, off, length, limit)
	}
	if err := ensure(frameHeader + length); err != nil {
		return nil, 0, err
	}
	p := w.rbuf[off-w.rbufOff+frameHeader : off-w.rbufOff+frameHeader+length]
	if crc32.Checksum(p, castagnoli) != crc {
		return nil, 0, fmt.Errorf("wal: порча сегмента %s@%d: CRC кадра не сходится — буфер повреждён вне восстановимого хвоста", path, off)
	}
	return p, off + frameHeader + length, nil
}

// WaitData блокирует читателя до появления новых durable-данных, стопа или
// фатала. Возвращает false, когда ждать больше нечего (стоп после дочитывания
// durable-границы при закрытом писателе).
func (w *WAL) WaitData(stop <-chan struct{}) bool {
	w.mu.Lock()
	backlog := w.backlogLocked()
	closed := w.closed
	w.mu.Unlock()
	if backlog {
		return true
	}
	if closed {
		return false
	}
	select {
	case <-w.dataCh:
		return true
	case <-stop:
		return true // владелец сам решит дочитывать ли durable-хвост
	case <-w.fatalCh:
		return true
	case <-time.After(250 * time.Millisecond):
		return true // страховочный тик
	}
}

// Ack подтверждает доставку всех кадров до pos включительно: курсор
// персистится (tmp+rename), полностью подтверждённые закрытые сегменты
// удаляются, заблокированные писатели будятся. Ошибка I/O — фатальна.
func (w *WAL) Ack(pos Pos) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cursor.Before(pos) {
		return nil
	}
	w.cursor = pos
	return w.reapLocked()
}

// reapLocked — уборка после продвижения курсора: нормализация (конец
// закрытого сегмента → начало следующего), срез меток возраста, персист
// курсора, удаление полностью подтверждённых закрытых сегментов, пробуждение
// заблокированных писателей. Активный (открытый) сегмент не удаляется
// никогда — его набор событий не финален; полностью подтверждённым он станет
// после ротации либо CloseWriter (после полного дренажа graceful-стоп
// оставляет пустой каталог буфера).
//
// Персист курсора идёт ДО удаления файлов: краш между ними оставит лишь
// orphan-сегменты, которые уберёт recover(); обратный порядок дал бы
// повторную доставку целого сегмента.
func (w *WAL) reapLocked() error {
	if s := w.segBySeq(w.cursor.Seg); s != nil && s.closed && w.cursor.Off >= s.size {
		w.cursor = Pos{Seg: s.seq + 1}
	}

	// Метки возраста до курсора больше не нужны.
	i := 0
	for i < len(w.ages) && !w.cursor.Before(w.ages[i].pos) {
		i++
	}
	w.ages = w.ages[i:]

	if err := w.persistCursorLocked(); err != nil {
		w.setFatalIO(err)
		return err
	}

	freed := false
	kept := w.segs[:0]
	for idx, s := range w.segs {
		if s.closed && s.seq < w.cursor.Seg {
			if w.rdSeq == s.seq && w.rd != nil {
				_ = w.rd.Close()
				w.rd = nil
			}
			if err := os.Remove(s.path); err != nil {
				err = fmt.Errorf("wal: удаление подтверждённого сегмента %s: %w", s.path, err)
				w.setFatalIO(err)
				w.segs = append(kept, w.segs[idx:]...) // хвост списка сохраняется как есть
				return err
			}
			w.total -= s.size
			freed = true
			continue
		}
		kept = append(kept, s)
	}
	w.segs = kept
	if freed {
		w.space.Broadcast()
	}
	return nil
}

func (w *WAL) persistCursorLocked() error {
	b, err := json.Marshal(w.cursor)
	if err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, cursorFn)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("wal: запись cursor.json: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("wal: подмена cursor.json: %w", err)
	}
	return nil
}

// Stats — снапшот для /metrics и прогресса: суммарный размер живых сегментов,
// их число и возраст старейшего durable-кадра, не подтверждённого БД (0 —
// backlog пуст).
func (w *WAL) Stats() (bytes int64, segments int, oldestUnacked time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	bytes = w.total
	segments = len(w.segs)
	if w.backlogLocked() && len(w.ages) > 0 {
		oldestUnacked = time.Since(w.ages[0].t)
		if oldestUnacked < 0 {
			oldestUnacked = 0
		}
	}
	return bytes, segments, oldestUnacked
}
