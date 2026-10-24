// Package board implements TaskView's platform-independent HTTP layer.
//
// The board serves the single-page UI, the /api/health endpoint and the
// /api/board REST surface. It deliberately depends only on taskview/internal/db
// and the standard library, so it builds and its tests run on Linux, macOS and
// Windows alike. The root taskview package keeps the platform-specific shell
// (tray, autostart, installer, browser launch) and forwards to this package.
package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"taskview/internal/db"
)

const maxBodyBytes = 5 * 1024 * 1024 // 5 MB

var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".txt":   "text/plain; charset=utf-8",
	".wasm":  "application/wasm",
}

// ServerOptions configures StartServer.
type ServerOptions struct {
	Port        int
	Host        string
	OpenBrowser bool
	DB          *db.Store
	FS          fs.FS
	MaxAttempts int
	WrapHandler func(http.Handler) http.Handler

	// Version is reported by /api/health. The root package passes its
	// build-time version in, so this package never keeps a second copy of it.
	Version string
}

// RunningServer holds an active HTTP server and its listening metadata.
type RunningServer struct {
	Server   *http.Server
	Listener net.Listener
	Port     int
	URL      string
	Store    *db.Store
}

// Close gracefully stops the server and releases listeners.
func (s *RunningServer) Close() error {
	if s == nil || s.Server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.Server.Shutdown(ctx)
	if err != nil {
		_ = s.Server.Close()
	}
	return err
}

// emptyFS makes a nil static filesystem behave like an asset that is simply
// missing instead of panicking. The root package resolves nil to the embedded
// assets before it calls into this package.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ContentTypeFor maps a file extension to its MIME type.
func ContentTypeFor(filePath string) string {
	ext := strings.ToLower(path.Ext(filePath))
	if ct, ok := mimeTypes[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

// SendJSON writes payload as JSON with the board's standard headers.
func SendJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"Internal server error","success":false}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// SendError writes a standard {"error": ..., "success": false} body.
func SendError(w http.ResponseWriter, status int, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "Error"
		}
	}
	SendJSON(w, status, map[string]any{
		"error":   msg,
		"success": false,
	})
}

func ParseEffectiveHostPort(rawHost, defaultScheme string) (host, port string, err error) {
	rawHost = strings.TrimSpace(rawHost)
	if rawHost == "" {
		return "", "", errors.New("empty host")
	}
	h, p, err := net.SplitHostPort(rawHost)
	if err != nil {
		h = rawHost
		if defaultScheme == "https" {
			p = "443"
		} else {
			p = "80"
		}
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	return h, p, nil
}

// ValidateHost accepts only loopback Host headers.
func ValidateHost(rawHost string) (host, port string, err error) {
	h, p, err := ParseEffectiveHostPort(rawHost, "http")
	if err != nil {
		return "", "", err
	}
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return h, p, nil
	default:
		return "", "", fmt.Errorf("invalid host %q", rawHost)
	}
}

// ValidateOrigin enforces the same-origin policy applied to every API request.
func ValidateOrigin(origin string, reqHost, reqPort string) (bool, string) {
	if origin == "" {
		return true, ""
	}
	if origin == "null" {
		return false, "Cross-origin requests with null origin are forbidden"
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false, "Invalid Origin header"
	}
	if u.Scheme != "http" {
		return false, "Non-HTTP Origin is forbidden"
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false, "Origin with path, query, or fragment is forbidden"
	}
	if u.User != nil {
		return false, "Origin credentials are forbidden"
	}
	origHost, origPort, err := ParseEffectiveHostPort(u.Host, u.Scheme)
	if err != nil {
		return false, "Invalid Origin host"
	}
	if origHost != reqHost || origPort != reqPort {
		return false, "Cross-origin requests are forbidden"
	}
	return true, ""
}

// ParseBoolParam parses the boolean query values the board accepts.
func ParseBoolParam(value string) (bool, bool) {
	v := strings.TrimSpace(strings.ToLower(value))
	switch v {
	case "1", "true", "yes", "y", "on":
		return true, true
	case "0", "false", "no", "n", "off":
		return false, true
	default:
		return false, false
	}
}

// ServeStaticAsset serves one request from staticFS.
func ServeStaticAsset(w http.ResponseWriter, r *http.Request, pathname string, staticFS fs.FS) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		SendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
		return
	}

	cleanPath := path.Clean(pathname)
	rel := strings.TrimPrefix(cleanPath, "/")
	if rel == "" || rel == "." {
		rel = "index.html"
	}

	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/..") {
		SendError(w, http.StatusForbidden, "Forbidden")
		return
	}

	f, err := staticFS.Open(rel)
	if err != nil {
		SendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		SendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
		return
	}

	if stat.IsDir() {
		indexRel := path.Join(rel, "index.html")
		indexFile, err := staticFS.Open(indexRel)
		if err != nil {
			SendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
			return
		}
		defer indexFile.Close()
		stat, err = indexFile.Stat()
		if err != nil {
			SendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
			return
		}
		f = indexFile
		rel = indexRel
	}

	ct := ContentTypeFor(rel)
	w.Header().Set("Content-Type", ct)
	ext := strings.ToLower(path.Ext(rel))
	if ext == ".html" || ext == ".htm" || ext == ".js" || ext == ".mjs" || ext == ".css" || ext == ".svg" || ext == ".json" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// CreateApp returns the HTTP handler for TaskView.
//
// staticFS supplies the UI assets (the root package passes its embedded
// src/public). version is reported by /api/health.
func CreateApp(store *db.Store, staticFS fs.FS, version string) http.Handler {
	if staticFS == nil {
		staticFS = emptyFS{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqHost, reqPort, err := ValidateHost(r.Host)
		if err != nil {
			SendError(w, http.StatusBadRequest, "Invalid Host header")
			return
		}

		origin := r.Header.Get("Origin")
		if origin != "" {
			valid, errMsg := ValidateOrigin(origin, reqHost, reqPort)
			if !valid {
				SendError(w, http.StatusForbidden, errMsg)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		parsedURL, err := url.Parse(r.RequestURI)
		if err != nil {
			SendError(w, http.StatusBadRequest, "Invalid request URL")
			return
		}

		cleanPath := path.Clean(parsedURL.Path)
		if cleanPath == "." {
			cleanPath = "/"
		}

		// API routes
		if cleanPath == "/api" || cleanPath == "/api/health" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				SendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
				return
			}
			payload := map[string]any{
				"success": true,
				"version": version,
			}
			// A health check that never touches the database would report a
			// healthy service that cannot serve a single task.
			if store == nil {
				payload["success"] = false
				payload["error"] = "database is not available"
				SendJSON(w, http.StatusServiceUnavailable, payload)
				return
			}
			if err := store.Ping(); err != nil {
				payload["success"] = false
				payload["error"] = "database is not readable: " + err.Error()
				SendJSON(w, http.StatusServiceUnavailable, payload)
				return
			}
			SendJSON(w, http.StatusOK, payload)
			return
		}

		if strings.HasPrefix(cleanPath, "/api/board/") || cleanPath == "/api/board" {
			handleBoardAPI(store, w, r, cleanPath)
			return
		}

		if strings.HasPrefix(cleanPath, "/api/") {
			SendError(w, http.StatusNotFound, fmt.Sprintf("Unknown API endpoint: %s", cleanPath))
			return
		}

		// Static UI assets
		ServeStaticAsset(w, r, cleanPath, staticFS)
	})
}

// IsAddressInUse reports whether err is a bind conflict.
func IsAddressInUse(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			if errors.Is(sysErr.Err, syscall.EADDRINUSE) {
				return true
			}
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}

// StartServer launches TaskView HTTP server binding strictly to 127.0.0.1.
func StartServer(opts ServerOptions) (*RunningServer, error) {
	if opts.DB == nil {
		return nil, errors.New("store must not be nil")
	}
	if opts.Host != "" && opts.Host != "127.0.0.1" && opts.Host != "localhost" {
		return nil, fmt.Errorf("external bind refused: host must be loopback (127.0.0.1 or localhost), got %q", opts.Host)
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("invalid port: %d (must be 0-65535)", opts.Port)
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 10
	}

	const host = "127.0.0.1"

	var ln net.Listener
	var err error
	actualPort := opts.Port

	if opts.Port == 0 {
		ln, err = net.Listen("tcp", fmt.Sprintf("%s:0", host))
		if err != nil {
			return nil, fmt.Errorf("listen on %s:0: %w", host, err)
		}
	} else {
		for attempt := 0; attempt <= opts.MaxAttempts; attempt++ {
			portToTry := opts.Port + attempt
			if portToTry > 65535 {
				return nil, fmt.Errorf("port fallback exceeded 65535: %d", portToTry)
			}
			ln, err = net.Listen("tcp", fmt.Sprintf("%s:%d", host, portToTry))
			if err == nil {
				actualPort = portToTry
				break
			}
			if !IsAddressInUse(err) || attempt == opts.MaxAttempts {
				return nil, fmt.Errorf("bind to port %d: %w", portToTry, err)
			}
		}
	}

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if ok {
		actualPort = tcpAddr.Port
	}
	actualURL := fmt.Sprintf("http://127.0.0.1:%d", actualPort)

	handler := CreateApp(opts.DB, opts.FS, opts.Version)
	if opts.WrapHandler != nil {
		handler = opts.WrapHandler(handler)
	}
	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		_ = srv.Serve(ln)
	}()

	if opts.OpenBrowser {
		OpenBrowser(actualURL)
	}

	return &RunningServer{
		Server:   srv,
		Listener: ln,
		Port:     actualPort,
		URL:      actualURL,
		Store:    opts.DB,
	}, nil
}
