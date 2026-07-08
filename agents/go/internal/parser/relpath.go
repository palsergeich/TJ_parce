// relpath.go — построение поля file_path: «ровно два уровня предков»,
// <коллекция>\<process_pid>\<файл>.log (format-spec §3). Используется обоими
// режимами (batch и follow), поэтому живёт в parser, а не в main.
package parser

import (
	"path/filepath"
	"strings"
)

// RelFilePath — «ровно два уровня предков»: <коллекция>\<process_pid>\<файл>.log
// (format-spec §3). Компоненты берутся из фактического пути; отсутствующий
// предок даёт пустую часть — композиция повторяет семантику fs::path::operator/.
func RelFilePath(path string) string {
	parent := filepath.Dir(path)
	grandparent := filepath.Dir(parent)
	return cppJoin(cppJoin(pathFilename(grandparent), pathFilename(parent)), filepath.Base(path))
}

// pathFilename — аналог fs::path::filename(): для корня диска возвращает "".
func pathFilename(p string) string {
	b := filepath.Base(p)
	if b == "." || b == string(filepath.Separator) || strings.HasSuffix(b, ":") {
		return ""
	}
	return b
}

// cppJoin — семантика fs::path::operator/ для относительных компонентов.
func cppJoin(p, x string) string {
	if x == "" {
		if p == "" {
			return ""
		}
		if !strings.HasSuffix(p, string(filepath.Separator)) {
			return p + string(filepath.Separator)
		}
		return p
	}
	if p == "" {
		return x
	}
	if strings.HasSuffix(p, string(filepath.Separator)) {
		return p + x
	}
	return p + string(filepath.Separator) + x
}
