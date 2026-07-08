//go:build !windows

// ident_other.go — POSIX-фолбэк открытия/идентичности (контракт допускает
// os.FileInfo.Sys: device + inode). Держит пакет собираемым и тестируемым
// вне Windows; целевая платформа bake-off — Windows.
package follow

import (
	"fmt"
	"os"
	"syscall"
)

func openShared(path string) (*os.File, error) { return os.Open(path) }

func fileIdentityOf(f *os.File) (identity, error) {
	fi, err := f.Stat()
	if err != nil {
		return identity{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, fmt.Errorf("%s: нет syscall.Stat_t в FileInfo.Sys()", f.Name())
	}
	return identity{Vol: uint64(st.Dev), Hi: 0, Lo: uint64(st.Ino)}, nil
}
