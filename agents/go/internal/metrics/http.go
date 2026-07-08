// http.go — HTTP-endpoint /metrics. Отдельный net/http-сервер на адресе из
// конфига (--metrics / metrics:); по умолчанию выключен. Только GET /metrics,
// прочие пути — 404.
package metrics

import (
	"bytes"
	"net"
	"net/http"
	"time"
)

// contentType — версия текстового формата Prometheus.
const contentType = "text/plain; version=0.0.4; charset=utf-8"

// nowLogClock — «сейчас» на оси времени техжурнала. Конвенция хранилища
// (format-spec, user-guide §10): метка события — ЛОКАЛЬНОЕ время
// сервера-источника, записанное как UTC. Агент работает на том же сервере,
// поэтому для честного lag стеночные часы агента отображаются на ту же ось:
// UTC-инстант + смещение зоны. Иначе lag был бы сдвинут на часовой пояс
// (при UTC+3 события «из будущего» зажимались бы в 0).
func nowLogClock() time.Time {
	now := time.Now()
	_, off := now.Zone()
	return now.Add(time.Duration(off) * time.Second).UTC()
}

// Handler — HTTP-хэндлер /metrics (снапшот реестра на момент запроса).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		RenderText(&b, nowLogClock())
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(b.Bytes())
	})
}

// StartServer поднимает HTTP-сервер метрик на addr (форматы host:port или
// :port; 0.0.0.0:порт — слушать все интерфейсы, нужно для скрейпа из
// Docker-контейнера Prometheus через host.docker.internal). Возвращает сервер
// (закрывать через Close) и фактический адрес (для addr с портом 0).
// Ошибка привязки (занятый порт и т.п.) возвращается сразу — fail-fast.
func StartServer(addr string) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }() // ErrServerClosed после Close — штатно
	return srv, ln.Addr().String(), nil
}
