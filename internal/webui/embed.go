package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:public
var Assets embed.FS

// Handler returns an http.Handler that serves the embedded web UI static assets.
func Handler() http.Handler {
	sub, err := fs.Sub(Assets, "public")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded assets carry no Last-Modified/ETag validators, so browsers
		// would cache JS/CSS heuristically and could run stale scripts against
		// new views after an upgrade. no-cache forces revalidation (a full
		// refetch here), keeping HTML and JS in lockstep.
		w.Header().Set("Cache-Control", "no-cache")

		upath := r.URL.Path
		if !strings.HasPrefix(upath, "/") {
			upath = "/" + upath
		}
		cleanPath := path.Clean(upath)
		trimmedPath := strings.TrimPrefix(cleanPath, "/")

		if trimmedPath == "" {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Try opening the requested file in embedded FS
		f, err := sub.Open(trimmedPath)
		if err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// Fallback to index.html for SPA routes
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
