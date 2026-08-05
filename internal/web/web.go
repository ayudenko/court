// Package web — минимальный веб-интерфейс наблюдателя: список дебатов
// и страница дискуссии с live-обновлением через SSE. Одна статическая
// страница, встроенная в бинарник.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed icon.svg
var iconSVG []byte

// Handler отдаёт страницу наблюдателя (роутинг по пути делает клиент).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(indexHTML)
	})
}

// IconHandler отдаёт favicon (SVG годится и для запросов /favicon.ico —
// браузеры смотрят на Content-Type).
func IconHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(iconSVG)
	})
}
