// worker.go — воркер-хвостовик: владеет закреплёнными за ним файлами
// (tailer'ами), дочитывает их хвосты, собирает события (assembler), строит
// строки и шлёт слабы батчеру chsink. Файл принадлежит ровно одному воркеру —
// порядок событий файла в потоке строк сохраняется (нужно для монотонного
// продвижения чекпоинта).
package follow

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"tjagent/internal/chsink"
	"tjagent/internal/metrics"
	"tjagent/internal/parser"
)

// tailer — состояние слежения за одним файлом (владеет только его воркер).
type tailer struct {
	fst        *fileState
	f          *os.File // nil — дремлет (хэндл закрыт, переоткроется по росту)
	asm        assembler
	target     int64 // последний размер от discovery
	lastData   time.Time
	queued     bool // уже стоит в очереди дочитывания воркера
	started    bool // чтение этого поколения начато (гейт MinFileSize пройден)
	openWarned bool

	datePrefix string
	filename   string
	filePath   string
	mc         *metrics.Coll // счётчики коллекции файла (/metrics)
}

// closeFile закрывает хэндл хвоста (единственная точка закрытия —
// gauge tj_agent_files_open остаётся согласованной).
func (t *tailer) closeFile() {
	if t.f == nil {
		return
	}
	_ = t.f.Close()
	t.f = nil
	metrics.FilesOpenAdd(-1)
}

type worker struct {
	in   chan sizeMsg
	stop <-chan struct{}

	reg       *registry
	sink      *chsink.Sink
	builder   *chsink.RowBuilder
	st        *stats
	idleClose time.Duration
	poll      time.Duration // каданс авторитетной проверки роста (--poll-ms)

	tailers     map[uint32]*tailer
	queue       []uint32
	slab        []chsink.Row
	readBuf     []byte
	stopped     bool
	sinkDead    bool
	lastGrowth  time.Time // последний growthSweep
	lastDormant time.Time // последний os.Stat-обход дремлющих
}

func (w *worker) run() {
	w.readBuf = make([]byte, tailReadChunk)
	tick := workerTick
	if w.poll < tick {
		tick = w.poll
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for !w.stopped {
		select {
		case m := <-w.in:
			w.observe(m)
		case <-ticker.C:
			w.idleSweep()
			if time.Since(w.lastGrowth) >= w.poll {
				w.lastGrowth = time.Now()
				w.growthSweep()
			}
		case <-w.stop:
			w.stopped = true
		}
		w.pump()
	}
	w.drainAll()
}

// observe — сигнал discovery о файле. Размер из сообщения НЕ авторитетен
// (обход каталога видит ленивые метаданные NTFS — см. growthSweep) и служит
// только пинком: точный размер process() берёт сам.
func (w *worker) observe(m sizeMsg) {
	t := w.tailers[m.id]
	if t == nil {
		fst := w.reg.file(m.id)
		base := filepath.Base(fst.path)
		fp := parser.RelFilePath(fst.path)
		t = &tailer{
			fst:        fst,
			datePrefix: parser.DateFromFilename(base),
			filename:   base,
			filePath:   fp,
			lastData:   time.Now(),
			mc:         metrics.GetColl(chsink.CollectionOf(fp)),
		}
		w.tailers[m.id] = t
	}
	if !t.queued {
		t.queued = true
		w.queue = append(w.queue, m.id)
	}
}

// growthSweep — авторитетная проверка роста раз в --poll-ms. Метаданные
// каталога NTFS для файлов, открытых писателем (rphost держит лог открытым
// на запись), обновляются ЛЕНИВО: FindFirstFile в обходе discovery видит
// замороженный размер, и по нему рост не detectится вовсе (вставки «копятся»
// и прорываются лишь после закрытия писателя). Поэтому рост отслеживаемых
// файлов проверяется точно: открытый хэндл → f.Stat()
// (GetFileInformationByHandle); дремлющий (хэндл закрыт) — раз в dormantPoll
// os.Stat по пути (он тоже открывает файл и точен, но дороже — CreateFile
// на каждый вызов). Обход discovery остаётся только каналом обнаружения
// НОВЫХ файлов.
func (w *worker) growthSweep() {
	dormantDue := time.Since(w.lastDormant) >= dormantPoll
	if dormantDue {
		w.lastDormant = time.Now()
	}
	for _, t := range w.tailers {
		size := int64(-1)
		switch {
		case t.f != nil:
			if fi, err := t.f.Stat(); err == nil {
				size = fi.Size()
			}
		case dormantDue:
			if fi, err := os.Stat(t.fst.path); err == nil {
				size = fi.Size()
			}
		}
		if size < 0 {
			continue
		}
		t.fst.size.Store(size)
		if size != t.asm.readOff() && !t.queued {
			t.queued = true
			w.queue = append(w.queue, t.fst.id)
		}
	}
}

// pump — обработка очереди дочитывания. Раунд файла ограничен maxRoundBytes:
// один гигантский файл (initial pass) не задерживает подхват новых у того же
// воркера. Между раундами вычерпываются свежие сообщения и проверяется stop.
func (w *worker) pump() {
	for len(w.queue) > 0 && !w.stopped {
		select {
		case m := <-w.in:
			w.observe(m)
			continue
		case <-w.stop:
			w.stopped = true
			return
		default:
		}
		id := w.queue[0]
		w.queue = w.queue[1:]
		t := w.tailers[id]
		t.queued = false
		w.process(t)
	}
}

// process — один раунд файла: авторитетный размер → усечение → гейт →
// открытие/идентичность → дочитывание хвоста → слабы. Не догнали за раунд —
// обратно в очередь.
func (w *worker) process(t *tailer) {
	if w.sinkDead {
		return
	}
	size, ok := w.authoritativeSize(t)
	if !ok {
		return // путь исчез между сигналом и проверкой — ждём следующего
	}
	t.target = size
	t.fst.size.Store(size)
	// Усечение: авторитетный размер меньше прочитанного → рестарт с нуля.
	if size < t.asm.readOff() {
		infof("[follow] %s: усечение (размер %d < прочитано %d) — чтение с нуля\n",
			t.fst.path, size, t.asm.readOff())
		w.resetTailer(t)
	}
	// Гейт MinFileSize=100 в начале поколения (свежий файл или после усечения):
	// перепроверяется по мере роста — 0-байтовый файл станет читаемым с 100 байт.
	if !t.started && size < parser.MinFileSize {
		return
	}
	if size <= t.asm.readOff() && t.f == nil {
		return // дремлет и не вырос — не открываем
	}
	if err := w.ensureOpen(t); err != nil {
		if !t.openWarned {
			fmt.Fprintf(os.Stderr, "[follow] не открыть %s: %v\n", t.fst.path, err)
			t.openWarned = true
			w.st.openErrs.Add(1)
		}
		return
	}
	t.openWarned = false

	emit := func(ev []byte, end int64) { w.emitEvent(t, ev, end) }
	round := 0
	for round < maxRoundBytes && !w.sinkDead {
		n, err := t.f.ReadAt(w.readBuf, t.asm.readOff())
		if n > 0 {
			t.started = true
			t.lastData = time.Now()
			w.st.bytes.Add(uint64(n))
			t.mc.ReadBytes.Add(uint64(n))
			t.asm.append(w.readBuf[:n], emit)
			t.fst.readOff.Store(t.asm.readOff())
			round += n
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "[follow] чтение %s: %v\n", t.fst.path, err)
				w.st.readErrs.Add(1)
			}
			break // EOF — хвост догнан
		}
	}
	if round >= maxRoundBytes && !w.stopped {
		// Ещё есть данные — дочитаем следующим раундом (справедливость).
		if !t.queued {
			t.queued = true
			w.queue = append(w.queue, t.fst.id)
		}
	}
	w.flushSlab() // строки не задерживаются у воркера (латентность хвоста)
}

// authoritativeSize — точный размер файла, минуя ленивый кэш каталога NTFS:
// открытый хэндл → f.Stat() (GetFileInformationByHandle), дремлющий →
// os.Stat по пути (открывает файл сам и тоже точен). Если хэндл роста не
// видит, дополнительная сверка по пути ловит подмену файла под тем же
// именем (пересоздание): расхождение размеров → хэндл закрывается,
// переоткрытие в ensureOpen сверит идентичность и начнёт с нуля.
func (w *worker) authoritativeSize(t *tailer) (int64, bool) {
	if t.f == nil {
		fi, err := os.Stat(t.fst.path)
		if err != nil {
			return 0, false
		}
		return fi.Size(), true
	}
	fi, err := t.f.Stat()
	if err != nil {
		return 0, false
	}
	size := fi.Size()
	if size == t.asm.readOff() {
		if fi2, err := os.Stat(t.fst.path); err == nil && fi2.Size() != size {
			t.closeFile()
			return fi2.Size(), true
		}
	}
	return size, true
}

// ensureOpen — открытие с полным шарингом + протокол идентичности:
// первое открытие → claim чекпоинта (резюме с committed, при size<committed —
// с нуля); переоткрытие → сверка идентичности, смена = пересоздание → с нуля.
func (w *worker) ensureOpen(t *tailer) error {
	if t.f != nil {
		return nil
	}
	f, err := openShared(t.fst.path)
	if err != nil {
		return err
	}
	id, err := fileIdentityOf(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	prev, claimed := t.fst.identSnapshot()
	switch {
	case !claimed:
		committed := w.reg.claim(t.fst, id)
		if committed > 0 {
			size := int64(-1)
			if fi, err := f.Stat(); err == nil {
				size = fi.Size()
			}
			if size < committed {
				// Контракт: текущий размер меньше чекпоинта → с нуля.
				infof("[follow] %s: размер %d < чекпоинта %d — чтение с нуля\n",
					t.fst.path, size, committed)
				w.reg.resetGen(t.fst)
				committed = 0
			}
		}
		if committed > 0 {
			t.asm.resumeAt(committed)
			t.started = true
			infof("[follow] %s: резюме с оффсета %d (чекпоинт)\n", t.fst.path, committed)
		}
		t.fst.readOff.Store(t.asm.readOff())
	case id != prev:
		// Файл пересоздан под тем же путём (ротация 1С) → с нуля.
		infof("[follow] %s: идентичность сменилась — файл пересоздан, чтение с нуля\n", t.fst.path)
		w.st.rebinds.Add(1)
		w.reg.rebind(t.fst, id)
		t.asm.reset()
		t.started = false
		t.fst.readOff.Store(0)
	}
	t.f = f
	metrics.FilesOpenAdd(1)
	return nil
}

// resetTailer — усечение: сброс сборки и чекпоинта, закрытие хэндла
// (при пересоздании старый хэндл смотрит в удалённый файл — переоткрытие
// в ensureOpen заодно сверит идентичность).
func (w *worker) resetTailer(t *tailer) {
	w.st.truncates.Add(1)
	t.asm.reset()
	t.started = false
	w.reg.resetGen(t.fst)
	t.fst.readOff.Store(0)
	t.closeFile()
}

// idleSweep — тик воркера: idle-закрытие событий (правило 2) и усыпление
// догнанных файлов (закрытие хэндла; переоткроется по следующему росту).
func (w *worker) idleSweep() {
	now := time.Now()
	for _, t := range w.tailers {
		if t.asm.inEvent && now.Sub(t.lastData) >= w.idleClose {
			if t.asm.idleEmit(func(ev []byte, end int64) { w.emitEvent(t, ev, end) }) {
				w.st.idleCloses.Add(1)
				t.fst.readOff.Store(t.asm.readOff())
				debugf("[follow] %s: событие закрыто по idle-таймауту\n", t.fst.path)
			}
		}
		if t.f != nil && t.target <= t.asm.readOff() && now.Sub(t.lastData) >= dormantAfter {
			t.closeFile()
			debugf("[follow] %s: хэндл закрыт (нет роста %v)\n", t.fst.path, dormantAfter)
		}
	}
	w.flushSlab()
}

// drainAll — graceful-стоп (правило 3): эмит \n-терминированных pending
// всех файлов, финальный слаб, закрытие хэндлов. Новые данные не читаются.
func (w *worker) drainAll() {
	for _, t := range w.tailers {
		t.asm.drain(func(ev []byte, end int64) { w.emitEvent(t, ev, end) })
		t.closeFile()
	}
	w.flushSlab()
}

// emitEvent — событие собрано: разбор полей, строка таблицы с меткой
// {файл, поколение, конец события} для продвижения чекпоинта по ack'у.
func (w *worker) emitEvent(t *tailer, ev []byte, end int64) {
	fld, ok := parser.ParseEventFields(ev)
	if !ok {
		// parse_skip: строки нет — байты события подтвердит End следующего
		// (перечитывание пропущенного при рестарте даёт снова пропуск, не дубль)
		w.st.parseSkips.Add(1)
		t.mc.ParseErrors.Add(1)
		return
	}
	row := w.builder.Build(fld, t.datePrefix, t.filename, t.filePath)
	row.Src = chsink.Src{File: t.fst.id, Gen: t.fst.genSnapshot(), End: end}
	w.slab = append(w.slab, row)
	w.st.events.Add(1)
	t.mc.Events.Add(1)
	// lag_seconds: максимальный ts обработанного события коллекции
	// (rich-схема валидирует ts сама; эпоха/деградация игнорируется в Observe).
	if row.Rich != nil {
		t.mc.ObserveEventTS(row.Rich.Time)
	} else {
		t.mc.ObserveEventTS(row.Time)
	}
	if len(w.slab) >= slabRows {
		w.flushSlab()
	}
}

// flushSlab — слаб батчеру. Блокирующая отправка — это и есть backpressure
// чтения при недоступном ClickHouse; Fatal (исчерпанные повторы после
// graceful-стопа) освобождает воркера, слаб отбрасывается — его строки не
// чекпоинтились, при следующем запуске перечитаются (потерь нет).
func (w *worker) flushSlab() {
	if len(w.slab) == 0 || w.sinkDead {
		w.slab = w.slab[:0]
		return
	}
	select {
	case w.sink.In() <- w.slab:
		w.slab = make([]chsink.Row, 0, slabRows)
	case <-w.sink.Fatal():
		w.sinkDead = true
		w.slab = w.slab[:0]
	}
}
