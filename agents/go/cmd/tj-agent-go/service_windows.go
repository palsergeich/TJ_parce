//go:build windows

// service_windows.go — служба Windows (golang.org/x/sys/windows/svc, запиннен
// в go.mod): установка/удаление/пуск/стоп через SCM и собственно сервисный
// цикл. Контракт:
//
//	tj-agent-go service install   --config <path> [--name tj-agent]
//	tj-agent-go service uninstall [--name tj-agent]
//	tj-agent-go service start     [--name tj-agent]
//	tj-agent-go service stop      [--name tj-agent]
//	tj-agent-go service run       --config <path> [--name tj-agent]
//
// install пишет команду запуска службы «<exe> service run --config <abs>»
// (StartType=Automatic) и требует прав администратора (mgr.Connect без них —
// Access is denied). `service run` вне SCM (svc.IsWindowsService()==false)
// работает как обычный консольный запуск `--config` с Ctrl+C — тот же код,
// что исполняет служба, проверяется без установки.
//
// Управляющий сигнал Stop/Shutdown от SCM закрывает канал остановки —
// это тот же graceful-дренаж follow-режима, что и stop-file: дочитанные
// \n-терминированные события эмитятся, финальный батч подтверждается,
// чекпоинты сохраняются. stop-file в конфиге службы поэтому опционален.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"tjagent/internal/agentcfg"
)

// defaultServiceName — имя службы по умолчанию (перекрывается --name:
// несколько агентов на одном хосте — разные имена и конфиги).
const defaultServiceName = "tj-agent"

func serviceUsage() {
	fmt.Fprint(os.Stderr,
		"Использование:\n"+
			"  tj-agent-go service install   --config <tj-agent.yaml> [--name tj-agent]\n"+
			"  tj-agent-go service uninstall [--name tj-agent]\n"+
			"  tj-agent-go service start     [--name tj-agent]\n"+
			"  tj-agent-go service stop      [--name tj-agent]\n"+
			"  tj-agent-go service run       --config <tj-agent.yaml> [--name tj-agent]\n"+
			"install/uninstall/start/stop требуют прав администратора.\n")
}

// serviceCommand — диспетчер подкоманды service (вызывает run() из main.go).
func serviceCommand(args []string) int {
	if len(args) == 0 {
		serviceUsage()
		return 1
	}
	sub := args[0]
	name := defaultServiceName
	cfgPath := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Ошибка: у флага --config нет значения")
				return 1
			}
			cfgPath = args[i+1]
			i++
		case "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Ошибка: у флага --name нет значения")
				return 1
			}
			name = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "Ошибка: неизвестный флаг %s\n", args[i])
			serviceUsage()
			return 1
		}
	}
	switch sub {
	case "install":
		return svcInstall(name, cfgPath)
	case "uninstall":
		return svcUninstall(name)
	case "start":
		return svcStart(name)
	case "stop":
		return svcStopCmd(name)
	case "run":
		return svcRun(name, cfgPath)
	default:
		fmt.Fprintf(os.Stderr, "Ошибка: неизвестная подкоманда service %q\n", sub)
		serviceUsage()
		return 1
	}
}

// connectSCM — подключение к менеджеру служб с человеческой диагностикой:
// без прав администратора OpenSCManager возвращает Access is denied.
func connectSCM() (*mgr.Mgr, bool) {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: подключение к менеджеру служб: %v\n", err)
		fmt.Fprintln(os.Stderr, "Управление службами требует прав администратора (запустите консоль от имени администратора).")
		return nil, false
	}
	return m, true
}

func svcInstall(name, cfgPath string) int {
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: service install требует --config <путь к YAML>")
		return 1
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: не разрешить путь %s: %v\n", cfgPath, err)
		return 1
	}
	// Конфиг валидируется ДО установки: служба с битым конфигом не поднимется,
	// а её stderr уходит в никуда — ошибку лучше увидеть здесь.
	if _, err := agentcfg.Load(abs); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: путь исполняемого файла: %v\n", err)
		return 1
	}
	m, ok := connectSCM()
	if !ok {
		return 1
	}
	defer m.Disconnect()
	if s, err := m.OpenService(name); err == nil {
		s.Close()
		fmt.Fprintf(os.Stderr, "Ошибка: служба %s уже установлена (сначала service uninstall)\n", name)
		return 1
	}
	s, err := m.CreateService(name, exe, mgr.Config{
		DisplayName: "Агент-сборщик техжурнала 1С (tj-agent-go)",
		Description: "Слежение за каталогами техжурнала 1С и доставка событий в ClickHouse (tj.events). Конфигурация: " + abs,
		StartType:   mgr.StartAutomatic,
	}, "service", "run", "--config", abs, "--name", name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: создание службы %s: %v\n", name, err)
		return 1
	}
	defer s.Close()
	// Источник журнала событий Windows — best-effort (диагностика старта до
	// открытия лог-файла); отказ не фатален: основной журнал — log_file.
	if err := eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		fmt.Fprintf(os.Stderr, "Предупреждение: источник журнала событий не зарегистрирован: %v\n", err)
	}
	fmt.Printf("Служба %s установлена: \"%s\" service run --config \"%s\"\nЗапуск: tj-agent-go service start --name %s\n", name, exe, abs, name)
	return 0
}

func svcUninstall(name string) int {
	m, ok := connectSCM()
	if !ok {
		return 1
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: служба %s не найдена: %v\n", name, err)
		return 1
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: удаление службы %s: %v\n", name, err)
		return 1
	}
	_ = eventlog.Remove(name)
	fmt.Printf("Служба %s удалена (работающий экземпляр доработает до остановки)\n", name)
	return 0
}

func svcStart(name string) int {
	m, ok := connectSCM()
	if !ok {
		return 1
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: служба %s не найдена: %v\n", name, err)
		return 1
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: запуск службы %s: %v\n", name, err)
		return 1
	}
	fmt.Printf("Служба %s запущена\n", name)
	return 0
}

// svcStopCmd — остановка с ожиданием: graceful-дренаж (финальный батч +
// чекпоинты) занимает до минуты при недоступном ClickHouse.
func svcStopCmd(name string) int {
	m, ok := connectSCM()
	if !ok {
		return 1
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: служба %s не найдена: %v\n", name, err)
		return 1
	}
	defer s.Close()
	st, err := s.Control(svc.Stop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: сигнал Stop службе %s: %v\n", name, err)
		return 1
	}
	deadline := time.Now().Add(2 * time.Minute)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "Ошибка: служба %s не остановилась за 2 минуты (состояние %d)\n", name, st.State)
			return 1
		}
		time.Sleep(300 * time.Millisecond)
		st, err = s.Query()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: опрос состояния службы %s: %v\n", name, err)
			return 1
		}
	}
	fmt.Printf("Служба %s остановлена (graceful-дренаж завершён)\n", name)
	return 0
}

// svcRun — вход `service run`. Под SCM (svc.IsWindowsService) — сервисный
// цикл; в консоли — обычный запуск --config (тот же код follow-режима,
// остановка Ctrl+C) для проверки конфигурации и конвейера без установки.
func svcRun(name, cfgPath string) int {
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: service run требует --config <путь к YAML>")
		return 1
	}
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: определение сервисного контекста: %v\n", err)
		return 1
	}
	if !isSvc {
		fmt.Fprintln(os.Stderr, "[service] процесс запущен не SCM — консольный режим (Ctrl+C для остановки)")
		return run([]string{"--config", cfgPath})
	}
	h := &svcHandler{cfgPath: cfgPath}
	if elog, err := eventlog.Open(name); err == nil {
		h.elog = elog
		defer elog.Close()
	}
	if err := svc.Run(name, h); err != nil {
		h.logErr("svc.Run: %v", err)
		return 1
	}
	return int(h.exitCode)
}

// svcHandler — обработчик управляющих сигналов SCM. Follow-режим крутится в
// горутине; Stop/Shutdown закрывает канал остановки (= stop-file) и ждёт
// полного дренажа перед докладом Stopped.
type svcHandler struct {
	cfgPath  string
	elog     *eventlog.Log
	exitCode uint32
}

func (h *svcHandler) logInfo(format string, a ...any) {
	if h.elog != nil {
		_ = h.elog.Info(1, fmt.Sprintf(format, a...))
	}
}

func (h *svcHandler) logErr(format string, a ...any) {
	if h.elog != nil {
		_ = h.elog.Error(1, fmt.Sprintf(format, a...))
	}
}

func (h *svcHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: 30_000}
	stopCh := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		// stderr службы уходит в никуда → журнал по умолчанию в
		// <state_dir>\tj-agent-go.log (см. runFollowFromConfigFile).
		done <- runFollowFromConfigFile(h.cfgPath, stopCh, true)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	h.logInfo("tj-agent запущен, конфигурация: %s", h.cfgPath)

	stopSent := false
	finish := func(code int) (bool, uint32) {
		h.exitCode = uint32(code)
		if code != 0 {
			h.logErr("tj-agent завершился с кодом %d (см. лог-файл агента)", code)
			return true, uint32(code) // service-specific exit code
		}
		h.logInfo("tj-agent остановлен штатно")
		return false, 0
	}
	for {
		select {
		case code := <-done:
			// Агент завершился сам: stop-file из конфига либо фатальная ошибка.
			status <- svc.Status{State: svc.StopPending}
			return finish(code)
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// Graceful-дренаж; WaitHint с запасом на ретраи вставки
				// (после SetDraining — ≤3 попытки с бэкоффом ≤2 с + вставка).
				status <- svc.Status{State: svc.StopPending, WaitHint: 120_000}
				if !stopSent {
					close(stopCh)
					stopSent = true
				}
				return finish(<-done)
			}
		}
	}
}
