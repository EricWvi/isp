package isp

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Frontend contains the production Vite build. Run npm run build in frontend
// before compiling the release binary.
//
//go:embed all:frontend/dist
var Frontend embed.FS

func FrontendHandler() (http.Handler, error) {
	files, err := fs.Sub(Frontend, "frontend/dist")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, err
	}
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if name != "." && name != "index.html" {
			if info, err := fs.Stat(files, name); err == nil && !info.IsDir() {
				static.ServeHTTP(w, r)
				return
			}
			if strings.Contains(path.Base(name), ".") {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	}), nil
}
