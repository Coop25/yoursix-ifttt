package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// staticHandler serves the embedded UI. Every response carries
// Cache-Control: no-cache plus an ETag of the file's content, so browsers
// revalidate on each load: a cheap 304 when unchanged, and the new files
// immediately after an upgrade instead of a stale UI.
func staticHandler(files fs.FS) http.Handler {
	etags := map[string]string{}
	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		etags[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	fileServer := http.FileServerFS(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		w.Header().Set("Cache-Control", "no-cache")
		if tag, ok := etags[name]; ok {
			// FileServer honours If-None-Match against this header.
			w.Header().Set("ETag", tag)
		}
		fileServer.ServeHTTP(w, r)
	})
}

// handleClearCache backs the UI's "Reload UI" button. Clear-Site-Data makes
// the browser drop everything it has cached for this origin. Only "cache":
// cookies and storage are left alone, so nobody is signed out. Browsers only
// honour it on HTTPS or localhost; the button also force-refetches the UI
// files for plain-HTTP LAN access.
func handleClearCache(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Clear-Site-Data", `"cache"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
