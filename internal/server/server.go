package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/session"
)

// Options configures the HTTP server.
type Options struct {
	// Addr is the listen address. It must be a loopback address: this process
	// reads production logs and makes no outbound connections, and binding it
	// to a public interface would undo that in one line.
	Addr string

	// Logger receives request lines. Nil means silence.
	Logger *log.Logger
}

// DefaultAddr binds loopback on a port unlikely to collide.
const DefaultAddr = "127.0.0.1:7717"

// Server exposes a session over HTTP for the web UI.
//
// Every endpoint calls the same session methods the CLI does. A capability
// reachable here but not from the terminal would be a bug, not a feature.
type Server struct {
	sess *session.Session
	opts Options
	mux  *http.ServeMux
}

// New builds a server over an open session.
func New(sess *session.Session, opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}

	s := &Server{sess: sess, opts: opts, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/schema", s.handleSchema)
	s.mux.HandleFunc("POST /api/query", s.handleQuery)
	s.mux.HandleFunc("POST /api/histogram", s.handleHistogram)
	s.mux.HandleFunc("GET /api/sources", s.handleSources)
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The UI is served from the same origin, so no cross-origin access is
	// needed or granted. A browser tab on another site must not be able to
	// read the contents of somebody's production logs.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	start := time.Now()
	s.mux.ServeHTTP(w, r)

	if s.opts.Logger != nil {
		s.opts.Logger.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	}
}

// Listen binds the address, refusing anything that is not loopback.
func (s *Server) Listen() (net.Listener, error) {
	host, _, err := net.SplitHostPort(s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", s.opts.Addr, err)
	}
	if err := requireLoopback(host); err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.opts.Addr, err)
	}
	return ln, nil
}

// requireLoopback refuses to bind anywhere reachable from the network.
//
// This is not defence in depth, it is the whole defence. There is no
// authentication and no authorisation, because this is a local tool reading
// local files; binding it to 0.0.0.0 would publish somebody's production logs
// to their office network. Refusing is better than documenting.
func requireLoopback(host string) error {
	if host == "" || host == "localhost" {
		return nil
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to bind to %q: only loopback addresses are allowed, "+
			"because loupe has no authentication and is reading your logs", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("refusing to bind to %s: only loopback addresses are allowed. "+
			"loupe has no authentication, so binding it to a reachable interface would "+
			"publish your logs. Use an SSH tunnel instead", host)
	}
	return nil
}

// Serve runs until the context is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler: s,
		// A query over a very large directory can be slow, but a request that
		// has not finished in two minutes is not going to.
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// writeJSON sends a value, or a plain 500 if it cannot be encoded.
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":"could not encode the response"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// apiError is the error shape every endpoint returns.
//
// The message is the one the CLI would print, unabridged. A filter typo
// deserves the same spelling suggestion in a browser as in a terminal, and
// flattening it to "bad request" would make the UI worse than the CLI at the
// exact moment the user needs help.
type apiError struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiError{Error: err.Error()})
}

// decodeBody reads a JSON request body with a size limit.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	const maxBody = 1 << 20

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
