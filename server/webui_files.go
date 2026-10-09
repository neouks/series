package server

import (
	"io/fs"
	"net/http"
	"strings"
)

// Next's build-addressed assets can be reused across navigations. HTML and route
// payloads must revalidate so a new deployment cannot keep an old chunk manifest.
func serveFileIfExists(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	st, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil || st.IsDir() {
		return false
	}
	if strings.HasPrefix(name, "_next/static/") {
		visibility := "public"
		if r.Context().Value(httpEntryContextKey{}) == true {
			// Shared caches must not serve a protected asset to anonymous clients.
			visibility = "private"
		}
		w.Header().Set("Cache-Control", visibility+", max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, fsys, name)
	return true
}
