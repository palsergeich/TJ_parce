// walglue.go — интеграция дискового буфера (internal/wal) в follow-конвейер.
//
// С buffer.type=disk конвейер: воркеры → NDJSON (parser.AppendEvent) →
// wal.Append → [fsync ≤ fsync_ms] → OnDurable → чекпоинты ТЖ; отдельный
// дренер (единственный читатель буфера) → BuildNDJSON (тот же rich/bench-
// маппинг и обогащение, что у прямого пути) → батчер chsink → ClickHouse →
// OnAck → wal.Ack (персист курсора, удаление подтверждённых сегментов).
//
// Гарантии: ТЖ→WAL — ноль потерь (чекпоинт только за fsync'нутые события);
// WAL→CH — at-least-once (реплей неподтверждённого хвоста после краша,
// дубли ограничены одним неподтверждённым батчем + одним fsync-окном).
// Фатальная ошибка I/O буфера жёстко останавливает агент (exit 3).
package follow

import (
	"fmt"

	"tjagent/internal/chsink"
	"tjagent/internal/wal"
)

// walExitCode — exit-код жёсткой остановки по фатальной ошибке буфера.
const walExitCode = 3

// srcFromPos кодирует позицию кадра буфера в chsink.Src строки реплея:
// File — младшие 32 бита номера сегмента, Gen — старшие, End — оффсет.
// Реестр файлов эти строки не видит (OnAck в WAL-режиме ведёт курсор буфера).
func srcFromPos(p wal.Pos) chsink.Src {
	return chsink.Src{File: uint32(p.Seg), Gen: uint32(p.Seg >> 32), End: p.Off}
}

func posFromSrc(s chsink.Src) wal.Pos {
	return wal.Pos{Seg: uint64(s.Gen)<<32 | uint64(s.File), Off: s.End}
}

// advanceDurable — продвижение чекпоинтов ТЖ после fsync буфера: семантика
// идентична onAck (max End в пределах поколения, немедленный сейв), но точка
// вызова — durable-запись на диск, а не подтверждение ClickHouse. Вызывается
// из-под мьютекса WAL — только реестр, без обращений к буферу.
func (r *registry) advanceDurable(entries []wal.Meta) {
	r.mu.RLock()
	files := r.files
	r.mu.RUnlock()
	for _, m := range entries {
		if int(m.File) >= len(files) {
			continue
		}
		fs := files[m.File]
		fs.mu.Lock()
		if fs.gen == m.Gen && m.End > fs.committed {
			fs.committed = m.End
		}
		fs.mu.Unlock()
	}
	r.dirty.Store(true)
	r.saveIfDirty()
}

// walAck — OnAck батчера в WAL-режиме: строки батча идут в порядке вставки,
// позиция последней — максимум; курсор буфера двигается до неё.
// Ошибку Ack (I/O курсора/удаления) фиксирует сам буфер (Fatal()).
func walAck(w *wal.WAL) func(rows []chsink.Row) {
	return func(rows []chsink.Row) {
		if len(rows) == 0 {
			return
		}
		_ = w.Ack(posFromSrc(rows[len(rows)-1].Src))
	}
}

// drainWAL — единственный читатель буфера: durable-кадры → строки → батчер.
// При graceful-стопе выходит после текущего слаба: остаток буфера durable
// и доставится при следующем запуске (быстрый стоп даже при лежащем CH).
// Ошибка декода кадра с валидным CRC = дрейф формата/баг → Fail (жёсткий
// стоп агента); фатал буфера обрабатывает Run.
func drainWAL(w *wal.WAL, sink *chsink.Sink, builder *chsink.RowBuilder, stop <-chan struct{}) {
	slab := make([]chsink.Row, 0, slabRows)
	send := func() bool {
		if len(slab) == 0 {
			return true
		}
		select {
		case sink.In() <- slab:
			slab = make([]chsink.Row, 0, slabRows)
			return true
		case <-sink.Fatal():
			return false
		}
	}
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	for {
		if stopped() {
			send()
			return
		}
		payload, pos, ok, err := w.TryNext()
		if err != nil {
			return // фатал буфера — Run жёстко останавливает агент
		}
		if !ok {
			if !send() {
				return
			}
			if !w.WaitData(stop) {
				return // писатель закрыт, durable-хвост дочитан
			}
			continue
		}
		row, derr := builder.BuildNDJSON(payload)
		if derr != nil {
			w.Fail(fmt.Errorf("кадр буфера с валидным CRC не декодируется (seg=%d off=%d): %w — дрейф версии формата NDJSON? Буфер записан другой ревизией агента",
				pos.Seg, pos.Off, derr))
			return
		}
		row.Src = srcFromPos(pos)
		slab = append(slab, row)
		if len(slab) >= slabRows {
			if !send() {
				return
			}
		}
	}
}
