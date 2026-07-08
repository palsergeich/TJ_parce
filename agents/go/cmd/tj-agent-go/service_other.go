//go:build !windows

// service_other.go — заглушка подкоманды service вне Windows: агент как
// демон официально поддерживается только службой Windows (target-платформа
// серверов 1С в этом проекте); на POSIX используйте systemd-юнит поверх
// «tj-agent-go --config <path>» (Ctrl+C/SIGINT — graceful-стоп).
package main

import (
	"fmt"
	"os"
)

func serviceCommand([]string) int {
	fmt.Fprintln(os.Stderr, "Ошибка: подкоманда service доступна только на Windows (POSIX: systemd поверх «tj-agent-go --config <path>»)")
	return 1
}
