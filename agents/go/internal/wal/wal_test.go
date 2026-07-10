// wal_test.go — юнит-тесты дискового буфера: кадры, CRC, восстановление
// битого хвоста, ротация сегментов, точный учёт размера, персист курсора,
// блокировка when_full, доставка OnDurable после fsync.
package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openT(t *testing.T, dir string, mut func(*Config)) *WAL {
	t.Helper()
	cfg := Config{
		Dir:          dir,
		MaxBytes:     1 << 30,
		SegmentBytes: 1 << 20,
		FsyncEvery:   50 * time.Millisecond,
		Logf:         t.Logf,
	}
	if mut != nil {
		mut(&cfg)
	}
	w, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

func payloadN(i, size int) []byte {
	p := make([]byte, size)
	copy(p, fmt.Sprintf(`{"n":%d}`, i))
	for j := range p {
		if p[j] == 0 {
			p[j] = byte('a' + (i+j)%26)
		}
	}
	return p
}

// readAll вычитывает все доступные durable-кадры (после CloseWriter — все).
func readAll(t *testing.T, w *WAL) (payloads [][]byte, last Pos) {
	t.Helper()
	for {
		p, pos, ok, err := w.TryNext()
		if err != nil {
			t.Fatalf("TryNext: %v", err)
		}
		if !ok {
			return payloads, last
		}
		cp := append([]byte(nil), p...)
		payloads = append(payloads, cp)
		last = pos
	}
}

func TestFrameRoundtrip(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, nil)
	defer w.Close()

	var want [][]byte
	for i := 0; i < 100; i++ {
		p := payloadN(i, 10+i*13)
		want = append(want, p)
		if err := w.Append(p, Meta{File: uint32(i), End: int64(i)}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatalf("CloseWriter: %v", err)
	}
	got, lastPos := readAll(t, w)
	if len(got) != len(want) {
		t.Fatalf("прочитано %d кадров, ожидалось %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != string(want[i]) {
			t.Fatalf("кадр %d не совпал", i)
		}
	}
	// Позиция последнего кадра == durable-конец.
	bytes, segs, _ := w.Stats()
	if segs != 1 {
		t.Fatalf("сегментов %d, ожидался 1", segs)
	}
	var sum int64
	for _, p := range want {
		sum += int64(len(p)) + frameHeader
	}
	if bytes != sum || lastPos.Off != sum {
		t.Fatalf("размер %d / последняя позиция %d, ожидалось %d", bytes, lastPos.Off, sum)
	}
}

func TestSegmentRotationAndOrder(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) { c.SegmentBytes = 4 << 10 })
	defer w.Close()

	const n = 200
	for i := 0; i < n; i++ {
		if err := w.Append(payloadN(i, 100), Meta{}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatalf("CloseWriter: %v", err)
	}
	_, segs, _ := w.Stats()
	if segs < 5 {
		t.Fatalf("ротация не состоялась: %d сегментов", segs)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*.wal"))
	if len(files) != segs {
		t.Fatalf("на диске %d сегментов, в учёте %d", len(files), segs)
	}
	for _, f := range files {
		fi, _ := os.Stat(f)
		if fi.Size() > 4<<10 {
			t.Fatalf("сегмент %s больше цели: %d Б", f, fi.Size())
		}
	}
	got, _ := readAll(t, w)
	if len(got) != n {
		t.Fatalf("прочитано %d, ожидалось %d", len(got), n)
	}
	for i, p := range got {
		if string(p[:len(fmt.Sprintf(`{"n":%d}`, i))]) != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("порядок кадров нарушен на %d: %q", i, p[:20])
		}
	}
}

func TestSizeAccountingExact(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) { c.SegmentBytes = 2 << 10 })
	defer w.Close()

	var expect int64
	for i := 0; i < 50; i++ {
		p := payloadN(i, 64+i)
		expect += int64(len(p)) + frameHeader
		if err := w.Append(p, Meta{}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		gotBytes, _, _ := w.Stats()
		if gotBytes != expect {
			t.Fatalf("после кадра %d: учтено %d Б, ожидалось %d", i, gotBytes, expect)
		}
	}
	// Дисковая сверка: Σ размеров файлов == учёт.
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	var onDisk int64
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*.wal"))
	for _, f := range files {
		fi, _ := os.Stat(f)
		onDisk += fi.Size()
	}
	if onDisk != expect {
		t.Fatalf("на диске %d Б, учтено %d", onDisk, expect)
	}

	// Подтверждение всего: писатель закрыт → ВСЕ сегменты удаляются, буфер
	// пустеет в ноль (чистый каталог после полного дренажа).
	_, last := readAll(t, w)
	if err := w.Ack(last); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	gotBytes, segs, oldest := w.Stats()
	if segs != 0 || gotBytes != 0 {
		t.Fatalf("после полного ack: %d сегментов, %d Б (ожидалось 0/0)", segs, gotBytes)
	}
	if oldest != 0 {
		t.Fatalf("oldest_unacked %v при пустом бэклоге", oldest)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "seg-*.wal")); len(left) != 0 {
		t.Fatalf("на диске остались сегменты: %v", left)
	}
}

func TestCRCCorruptionDetectedLazily(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) { c.SegmentBytes = 1 << 10 })
	for i := 0; i < 30; i++ {
		if err := w.Append(payloadN(i, 100), Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Портим байт полезной нагрузки в ПЕРВОМ (не последнем) сегменте:
	// recovery его не сканирует, порчу обязан поймать ленивый CRC читателя.
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*.wal"))
	if len(files) < 2 {
		t.Fatalf("нужно ≥2 сегментов, есть %d", len(files))
	}
	f, err := os.OpenFile(files[0], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, frameHeader+10); err != nil {
		t.Fatal(err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, frameHeader+10); err != nil {
		t.Fatal(err)
	}
	f.Close()

	w2 := openT(t, dir, func(c *Config) { c.SegmentBytes = 1 << 10 })
	defer w2.Close()
	_, _, _, err = w2.TryNext()
	if err == nil {
		t.Fatal("порча CRC в закрытом сегменте не обнаружена")
	}
	select {
	case <-w2.Fatal():
	default:
		t.Fatal("Fatal() не закрыт после порчи CRC")
	}
}

func TestTornTailRecovery(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, nil)
	var want [][]byte
	for i := 0; i < 10; i++ {
		p := payloadN(i, 200)
		want = append(want, p)
		if err := w.Append(p, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Дописываем рваный хвост: валидный заголовок с длиной больше остатка +
	// мусор (имитация краша посреди записи кадра).
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*.wal"))
	f, err := os.OpenFile(files[len(files)-1], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat()
	validSize := fi.Size()
	garbage := payloadN(99, 300)
	var hdr [frameHeader]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 1000) // длина больше фактических данных
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(garbage, castagnoli))
	if _, err := f.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(garbage[:150]); err != nil { // недописанный кадр
		t.Fatal(err)
	}
	f.Close()

	w2 := openT(t, dir, nil)
	defer w2.Close()
	fi2, _ := os.Stat(files[len(files)-1])
	if fi2.Size() != validSize {
		t.Fatalf("хвост не усечён: %d Б, ожидалось %d", fi2.Size(), validSize)
	}
	// Валидный префикс жив и читается; новые записи принимаются.
	extra := payloadN(1000, 123)
	if err := w2.Append(extra, Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := w2.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(t, w2)
	if len(got) != len(want)+1 {
		t.Fatalf("после восстановления прочитано %d кадров, ожидалось %d", len(got), len(want)+1)
	}
	for i := range want {
		if string(got[i]) != string(want[i]) {
			t.Fatalf("кадр %d повреждён после восстановления", i)
		}
	}
	if string(got[len(want)]) != string(extra) {
		t.Fatal("дозаписанный после восстановления кадр не совпал")
	}
}

func TestTornTailGarbageLength(t *testing.T) {
	// Мусор вместо заголовка (длина 0 / гигантская) — усечение по санити-чеку.
	dir := t.TempDir()
	w := openT(t, dir, nil)
	if err := w.Append(payloadN(1, 100), Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*.wal"))
	f, _ := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0)
	fi, _ := f.Stat()
	valid := fi.Size()
	_, _ = f.Write(make([]byte, 500)) // нули: длина 0 → мусор
	f.Close()

	w2 := openT(t, dir, nil)
	defer w2.Close()
	fi2, _ := os.Stat(files[0])
	if fi2.Size() != valid {
		t.Fatalf("хвост с мусорной длиной не усечён: %d, ожидалось %d", fi2.Size(), valid)
	}
}

func TestCursorPersistenceRoundtrip(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) { c.SegmentBytes = 1 << 10 })
	var want [][]byte
	for i := 0; i < 20; i++ {
		p := payloadN(i, 150)
		want = append(want, p)
		if err := w.Append(p, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	// Читаем 7, подтверждаем 7.
	var pos7 Pos
	for i := 0; i < 7; i++ {
		_, pos, ok, err := w.TryNext()
		if err != nil || !ok {
			t.Fatalf("TryNext %d: ok=%v err=%v", i, ok, err)
		}
		pos7 = pos
	}
	if err := w.Ack(pos7); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Рестарт: чтение продолжается с 8-го кадра, подтверждённые не реплеятся.
	w2 := openT(t, dir, func(c *Config) { c.SegmentBytes = 1 << 10 })
	defer w2.Close()
	if err := w2.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(t, w2)
	if len(got) != len(want)-7 {
		t.Fatalf("после рестарта прочитано %d, ожидалось %d", len(got), len(want)-7)
	}
	for i, p := range got {
		if string(p) != string(want[i+7]) {
			t.Fatalf("кадр %d после рестарта не совпал (реплей с неверного курсора)", i)
		}
	}
}

func TestAckDeletesSegmentsAndUnblocks(t *testing.T) {
	dir := t.TempDir()
	// Кап на 3 небольших кадра; сегмент меньше капа.
	w := openT(t, dir, func(c *Config) {
		c.MaxBytes = 3 * (128 + frameHeader)
		c.SegmentBytes = 128 + frameHeader
	})
	defer w.Close()

	for i := 0; i < 3; i++ {
		if err := w.Append(payloadN(i, 128), Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	// Буфер полон: Append обязан заблокироваться.
	done := make(chan error, 1)
	go func() { done <- w.Append(payloadN(3, 128), Meta{}) }()
	select {
	case err := <-done:
		t.Fatalf("Append не заблокировался на полном буфере (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Подтверждаем первый кадр → его сегмент удаляется → писатель разблокирован.
	p, pos, ok, err := w.TryNext()
	if err != nil || !ok || len(p) == 0 {
		t.Fatalf("TryNext: ok=%v err=%v", ok, err)
	}
	if err := w.Ack(pos); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Append после освобождения места: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Append не разблокировался после Ack")
	}
	bytes, _, _ := w.Stats()
	if bytes > w.cfg.MaxBytes {
		t.Fatalf("превышен кап: %d > %d", bytes, w.cfg.MaxBytes)
	}
}

func TestBeginShutdownReleasesBlockedAppend(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) {
		c.MaxBytes = 2 * (128 + frameHeader)
		c.SegmentBytes = 128 + frameHeader
	})
	defer w.Close()
	for i := 0; i < 2; i++ {
		if err := w.Append(payloadN(i, 128), Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- w.Append(payloadN(2, 128), Meta{}) }()
	time.Sleep(100 * time.Millisecond)
	w.BeginShutdown()
	select {
	case err := <-done:
		if err != ErrStopped {
			t.Fatalf("ожидался ErrStopped, получено %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BeginShutdown не отпустил заблокированный Append")
	}
}

func TestOnDurableAfterFsyncOnly(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []Meta
	w := openT(t, dir, func(c *Config) {
		c.FsyncEvery = 40 * time.Millisecond
		c.OnDurable = func(ms []Meta) {
			mu.Lock()
			got = append(got, ms...)
			mu.Unlock()
		}
	})
	defer w.Close()

	for i := 0; i < 5; i++ {
		if err := w.Append(payloadN(i, 100), Meta{File: 7, Gen: 1, End: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	// До первого тика fsync метаданные не доставляются (обычно мгновенно
	// после Append ничего нет; допускаем гонку тика — проверяем только
	// итоговую доставку и порядок).
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("OnDurable доставил %d из 5 за 2 с", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, m := range got {
		if m.End != int64(i+1) || m.File != 7 || m.Gen != 1 {
			t.Fatalf("метаданные не в порядке записи: %d → %+v", i, m)
		}
	}
}

func TestOnDurableDeliveredByCloseWriter(t *testing.T) {
	dir := t.TempDir()
	var got []Meta
	w := openT(t, dir, func(c *Config) {
		c.FsyncEvery = time.Hour // таймер не успеет — доставку обязан дать CloseWriter
		c.OnDurable = func(ms []Meta) { got = append(got, ms...) }
	})
	defer w.Close()
	if err := w.Append(payloadN(1, 50), Meta{End: 42}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("OnDurable до fsync")
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].End != 42 {
		t.Fatalf("CloseWriter не доставил метаданные: %+v", got)
	}
}

func TestOrphanAckedSegmentsCleanedOnOpen(t *testing.T) {
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) { c.SegmentBytes = 1 << 10 })
	for i := 0; i < 20; i++ {
		if err := w.Append(payloadN(i, 150), Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	// Подтверждаем всё; удаление сегментов сработает штатно.
	_, last := readAll(t, w)
	if err := w.Ack(last); err != nil {
		t.Fatal(err)
	}
	_, segsLeft, _ := w.Stats()
	w.Close()

	// Рестарт: ничего не реплеится.
	w2 := openT(t, dir, func(c *Config) { c.SegmentBytes = 1 << 10 })
	defer w2.Close()
	if err := w2.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(t, w2)
	if len(got) != 0 {
		t.Fatalf("после полного ack реплей %d кадров (сегментов оставалось %d)", len(got), segsLeft)
	}
}

func TestReadWhileWriting(t *testing.T) {
	// Конкурентный писатель + читатель: читатель видит только durable-кадры,
	// все кадры доходят без порчи и в порядке.
	dir := t.TempDir()
	w := openT(t, dir, func(c *Config) {
		c.SegmentBytes = 8 << 10
		c.FsyncEvery = 5 * time.Millisecond
	})
	defer w.Close()

	const n = 500
	go func() {
		for i := 0; i < n; i++ {
			_ = w.Append([]byte(fmt.Sprintf(`{"i":%06d}`, i)), Meta{})
		}
		_ = w.CloseWriter()
	}()

	stop := make(chan struct{})
	var seen int
	for seen < n {
		p, _, ok, err := w.TryNext()
		if err != nil {
			t.Fatalf("TryNext: %v", err)
		}
		if !ok {
			if !w.WaitData(stop) {
				break
			}
			continue
		}
		want := fmt.Sprintf(`{"i":%06d}`, seen)
		if string(p) != want {
			t.Fatalf("кадр %d: %q != %q", seen, p, want)
		}
		seen++
	}
	if seen != n {
		t.Fatalf("дочитано %d из %d", seen, n)
	}
}
