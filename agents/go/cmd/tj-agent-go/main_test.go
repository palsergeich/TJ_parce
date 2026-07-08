package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestKI14WalkErrorZeroFiles — KI-14 (format-spec §7): ошибки обхода каталогов
// при нуле найденных .log-файлов обязаны давать exit 2 и попадать в счётчик
// ошибок (раньше — ложный успех exit 0). Ошибку обхода моделирует каталог
// с запретом листинга (deny ReadData для Everyone, SID S-1-1-0 — не зависит
// от локали Windows).
func TestKI14WalkErrorZeroFiles(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-специфичный тест (icacls)")
	}
	tmp := t.TempDir()
	denied := filepath.Join(tmp, "denied")
	if err := os.Mkdir(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", denied, "/deny", "*S-1-1-0:(RD)").CombinedOutput(); err != nil {
		t.Skipf("icacls deny недоступен: %v: %s", err, out)
	}
	defer exec.Command("icacls", denied, "/remove:d", "*S-1-1-0").Run() //nolint:errcheck
	if _, err := os.ReadDir(denied); err == nil {
		t.Skip("запрет листинга не подействовал (backup-привилегии процесса?)")
	}

	outFile := filepath.Join(tmp, "out.jsonl")
	if code := run([]string{tmp, "1", outFile}); code != 2 {
		t.Errorf("exit = %d, want 2 (KI-14: ошибка обхода при нуле файлов)", code)
	}
	// Файлов не нашлось → выходной файл не создаётся (спека §6)
	if _, err := os.Stat(outFile); err == nil {
		t.Error("выходной файл не должен создаваться при нуле найденных файлов")
	}

	// Контроль: пустой каталог без ошибок обхода — по-прежнему exit 0
	clean := t.TempDir()
	if code := run([]string{clean, "1", filepath.Join(clean, "out.jsonl")}); code != 0 {
		t.Errorf("exit = %d, want 0 (нет файлов и нет ошибок)", code)
	}
}

// TestKI14StatsCounted — ошибка обхода отражается в failed_files stats-json.
func TestKI14StatsCounted(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-специфичный тест (icacls)")
	}
	tmp := t.TempDir()
	denied := filepath.Join(tmp, "denied")
	if err := os.Mkdir(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", denied, "/deny", "*S-1-1-0:(RD)").CombinedOutput(); err != nil {
		t.Skipf("icacls deny недоступен: %v: %s", err, out)
	}
	defer exec.Command("icacls", denied, "/remove:d", "*S-1-1-0").Run() //nolint:errcheck
	if _, err := os.ReadDir(denied); err == nil {
		t.Skip("запрет листинга не подействовал")
	}

	var s stats
	files := findLogFiles(tmp, &s)
	if len(files) != 0 {
		t.Fatalf("нашлись файлы: %v", files)
	}
	if s.failed.Load() == 0 {
		t.Error("ошибка обхода не попала в счётчик failed")
	}
}
