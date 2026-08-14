package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the built frontend.
//
// The directory carries a committed .gitkeep so that `go build` works on a
// fresh clone before anyone has run `make web`; the built assets themselves are
// not committed, per CLAUDE.md. A binary built without them still runs and
// still serves the API — it just explains how to build the UI instead of
// serving it.
//
//go:embed all:dist
var dist embed.FS

// uiFS returns the built assets, and whether there are any.
func uiFS() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}

// handleUI serves the single-page app.
//
// Any path that is not an asset returns index.html, so a reload or a deep link
// lands on the app rather than a 404. /api is routed before this and is never
// reached here.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	assets, ok := uiFS()
	if !ok {
		s.handleMissingUI(w, r)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	if f, err := assets.Open(path); err == nil {
		f.Close()
		// The UI is embedded and versioned with the binary, so nothing here
		// benefits from a cache that could serve yesterday's build.
		w.Header().Set("Cache-Control", "no-cache")
		http.FileServerFS(assets).ServeHTTP(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		s.handleMissingUI(w, r)
		return
	}
	w.Write(index)
}

// handleMissingUI explains a binary built without the frontend.
//
// Failing with a bare 404 would look like a bug in the tool rather than a build
// step nobody ran.
func (s *Server) handleMissingUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(missingUIPage))
}

const missingUIPage = `<!DOCTYPE html>
<meta charset="utf-8">
<title>loupe — UI not built</title>
<style>
  body{background:#131920;color:#c6d1dc;font:14px/1.7 ui-monospace,Menlo,monospace;
       margin:0;display:grid;place-items:center;height:100vh}
  div{max-width:44rem;padding:2rem}
  h1{font-size:15px;font-weight:600;margin:0 0 1rem}
  code{color:#5b8cad}
  p{color:#7d8d9c}
</style>
<div>
  <h1>This binary was built without the web UI.</h1>
  <p>The API is running and works. To build the interface:</p>
  <p><code>make web &amp;&amp; make build</code></p>
  <p>Or use the API directly:<br>
     <code>curl -s localhost:7717/api/schema | jq</code></p>
  <p>Or stay in the terminal: <code>loupe ./logs 'level:&gt;=error'</code></p>
</div>
`

// UIAvailable reports whether this binary carries a built frontend, so the
// serve command can say so rather than pointing at a page that will not load.
func UIAvailable() bool {
	_, ok := uiFS()
	return ok
}
