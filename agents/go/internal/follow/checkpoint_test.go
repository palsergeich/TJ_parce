package follow

import (
	"os"
	"path/filepath"
	"testing"
)

// Раундтрип: save → load → take; непривязанные записи переживают сохранение.
func TestCheckpointRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoints.json")

	cp := loadCheckpoints(path) // файла нет — чистый старт
	if got := cp.take(identity{Vol: 1, Lo: 2}); got != 0 {
		t.Fatalf("take на пустом сторе: %d", got)
	}
	recs := []checkpointRec{
		{Path: `E:\logs\rphost_1\25070812.log`, Identity: identity{Vol: 77, Hi: 1, Lo: 42}, Committed: 12345},
		{Path: `E:\logs\rphost_2\25070812.log`, Identity: identity{Vol: 77, Hi: 0, Lo: 43}, Committed: 500},
	}
	if err := cp.save(recs); err != nil {
		t.Fatal(err)
	}

	cp2 := loadCheckpoints(path)
	if got := cp2.take(identity{Vol: 77, Hi: 1, Lo: 42}); got != 12345 {
		t.Errorf("take = %d, want 12345", got)
	}
	// Повторный take той же идентичности — запись уже забрана
	if got := cp2.take(identity{Vol: 77, Hi: 1, Lo: 42}); got != 0 {
		t.Errorf("повторный take = %d, want 0", got)
	}
	// Незабранная запись включается в следующий снапшот (leftovers)
	if err := cp2.save(nil); err != nil {
		t.Fatal(err)
	}
	cp3 := loadCheckpoints(path)
	if got := cp3.take(identity{Vol: 77, Hi: 0, Lo: 43}); got != 500 {
		t.Errorf("leftover потерян: take = %d, want 500", got)
	}
	if got := cp3.take(identity{Vol: 77, Hi: 1, Lo: 42}); got != 0 {
		t.Errorf("забранная запись воскресла: %d", got)
	}
}

// Повреждённый файл чекпоинтов — чистый старт, не паника.
func TestCheckpointCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoints.json")
	if err := os.WriteFile(path, []byte("{битый json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := loadCheckpoints(path)
	if got := cp.take(identity{Vol: 1}); got != 0 {
		t.Errorf("take = %d", got)
	}
}

// Идентичность файла стабильна между открытиями и различает разные файлы.
func TestFileIdentity(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.log")
	p2 := filepath.Join(dir, "b.log")
	if err := os.WriteFile(p1, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	open := func(p string) identity {
		f, err := openShared(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		id, err := fileIdentityOf(f)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id1a, id1b, id2 := open(p1), open(p1), open(p2)
	if id1a != id1b {
		t.Errorf("идентичность нестабильна: %+v != %+v", id1a, id1b)
	}
	if id1a == id2 {
		t.Errorf("разные файлы с одной идентичностью: %+v", id1a)
	}
	// Пересоздание под тем же путём меняет идентичность (file index)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id1c := open(p1); id1c == id1a {
		t.Logf("ВНИМАНИЕ: идентичность совпала после пересоздания (%+v) — ФС переиспользовала file index", id1c)
	}
}

// Файл, открытый нами с полным шарингом, может быть удалён и переименован
// другим процессом (FILE_SHARE_DELETE) — критично для ротации rphost.
func TestOpenSharedAllowsDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "live.log")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, filepath.Join(dir, "rotated.log")); err != nil {
		t.Errorf("rename при открытом хэндле: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "rotated.log")); err != nil {
		t.Errorf("delete при открытом хэндле: %v", err)
	}
	// Чтение по старому хэндлу продолжает работать
	buf := make([]byte, 4)
	if n, err := f.ReadAt(buf, 0); err != nil || n != 4 {
		t.Errorf("чтение после удаления: n=%d err=%v", n, err)
	}
}
