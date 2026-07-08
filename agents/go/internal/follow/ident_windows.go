//go:build windows

// ident_windows.go — открытие с ПОЛНЫМ шарингом и идентичность файла.
//
// rphost 1С держит текущий лог открытым на запись; чтобы не мешать ему
// писать/ротировать/удалять, читатель обязан открывать файл с
// FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE. Стандартный os.Open
// в Go не передаёт FILE_SHARE_DELETE — поэтому CreateFile напрямую.
package follow

import (
	"os"
	"strings"
	"syscall"
)

// openShared открывает файл только на чтение с полным шарингом.
func openShared(path string) (*os.File, error) {
	p := path
	if len(p) >= 248 && !strings.HasPrefix(p, `\\?\`) {
		// Длинные пути: \\?\-префикс (пути discovery абсолютные и чистые)
		if strings.HasPrefix(p, `\\`) {
			p = `\\?\UNC` + p[1:]
		} else {
			p = `\\?\` + p
		}
	}
	namep, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(namep,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// fileIdentityOf — идентичность открытого файла: volume serial + file index
// (GetFileInformationByHandle). Имя файла в идентичность не входит — 1С
// переиспользует имена YYMMDDHH.log при ротации.
func fileIdentityOf(f *os.File) (identity, error) {
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return identity{}, &os.PathError{Op: "GetFileInformationByHandle", Path: f.Name(), Err: err}
	}
	return identity{
		Vol: uint64(d.VolumeSerialNumber),
		Hi:  uint64(d.FileIndexHigh),
		Lo:  uint64(d.FileIndexLow),
	}, nil
}
