package main

import (
	"bytes"
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
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	DB          *Store
	FS          fs.FS
	MaxAttempts int
	WrapHandler func(http.Handler) http.Handler
}

// RunningServer holds an active HTTP server and its listening metadata.
type RunningServer struct {
	Server   *http.Server
	Listener net.Listener
	Port     int
	URL      string
	Store    *Store
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

func contentTypeFor(filePath string) string {
	ext := strings.ToLower(path.Ext(filePath))
	if ct, ok := mimeTypes[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

func sendJSON(w http.ResponseWriter, status int, payload any) {
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

func sendError(w http.ResponseWriter, status int, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "Error"
		}
	}
	sendJSON(w, status, map[string]any{
		"error":   msg,
		"success": false,
	})
}

func parseEffectiveHostPort(rawHost, defaultScheme string) (host, port string, err error) {
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

func validateHost(rawHost string) (host, port string, err error) {
	h, p, err := parseEffectiveHostPort(rawHost, "http")
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

func validateOrigin(origin string, reqHost, reqPort string) (bool, string) {
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
	origHost, origPort, err := parseEffectiveHostPort(u.Host, u.Scheme)
	if err != nil {
		return false, "Invalid Origin host"
	}
	if origHost != reqHost || origPort != reqPort {
		return false, "Cross-origin requests are forbidden"
	}
	return true, ""
}

func readJSONBody(r *http.Request) (map[string]any, error) {
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, &ValidationError{Message: "Failed to read request body"}
	}
	if len(data) > maxBodyBytes {
		return nil, &ValidationError{Message: "Request body too large"}
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, &ValidationError{Message: "Request body must be valid JSON"}
	}

	// Check for trailing garbage after first JSON token
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, &ValidationError{Message: "Request body must be valid JSON"}
	}

	obj, ok := raw.(map[string]any)
	if !ok || raw == nil {
		return nil, &ValidationError{Message: "Request body must be a JSON object"}
	}

	return obj, nil
}

func parseBoolParam(value string) (bool, bool) {
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

func normalizeAPIProjectPath(val string) string {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" || trimmed == "all" {
		return ""
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return trimmed
	}
	return filepath.Clean(abs)
}

var writableTaskFields = map[string]bool{
	"title":        true,
	"description":  true,
	"status":       true,
	"priority":     true,
	"file_path":    true,
	"filePath":     true,
	"line_start":   true,
	"lineStart":    true,
	"line_end":     true,
	"lineEnd":      true,
	"code_snippet": true,
	"codeSnippet":  true,
	"agent_notes":  true,
	"agentNotes":   true,
	"project_path": true,
	"projectPath":  true,
	"project_name": true,
	"projectName":  true,
}

func pickWritable(body map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range body {
		if writableTaskFields[k] {
			out[k] = v
		}
	}
	return out
}

// OpenBrowser launches url in the default system browser without shell invocation.
func OpenBrowser(targetURL string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	case "darwin":
		cmd = exec.Command("open", targetURL)
	default:
		cmd = exec.Command("xdg-open", targetURL)
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() {
		_ = cmd.Wait()
	}()
	return true
}

func serveStaticAsset(w http.ResponseWriter, r *http.Request, pathname string, staticFS fs.FS) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
		return
	}

	cleanPath := path.Clean(pathname)
	rel := strings.TrimPrefix(cleanPath, "/")
	if rel == "" || rel == "." {
		rel = "index.html"
	}

	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/..") {
		sendError(w, http.StatusForbidden, "Forbidden")
		return
	}

	f, err := staticFS.Open(rel)
	if err != nil {
		sendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		sendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
		return
	}

	if stat.IsDir() {
		indexRel := path.Join(rel, "index.html")
		indexFile, err := staticFS.Open(indexRel)
		if err != nil {
			sendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
			return
		}
		defer indexFile.Close()
		stat, err = indexFile.Stat()
		if err != nil {
			sendError(w, http.StatusNotFound, fmt.Sprintf("Not found: %s", pathname))
			return
		}
		f = indexFile
		rel = indexRel
	}

	ct := contentTypeFor(rel)
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
func CreateApp(store *Store, staticFS fs.FS) http.Handler {
	if staticFS == nil {
		staticFS = getPublicFS()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqHost, reqPort, err := validateHost(r.Host)
		if err != nil {
			sendError(w, http.StatusBadRequest, "Invalid Host header")
			return
		}

		origin := r.Header.Get("Origin")
		if origin != "" {
			valid, errMsg := validateOrigin(origin, reqHost, reqPort)
			if !valid {
				sendError(w, http.StatusForbidden, errMsg)
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
			sendError(w, http.StatusBadRequest, "Invalid request URL")
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
				sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
				return
			}
			dbPath := ""
			if store != nil {
				dbPath = store.FilePath
			}
			sendJSON(w, http.StatusOK, map[string]any{"success": true, "db": dbPath})
			return
		}

		if cleanPath == "/api/projects" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
				return
			}
			projects, err := store.GetProjects()
			if err != nil {
				sendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if projects == nil {
				projects = []Project{}
			}
			sendJSON(w, http.StatusOK, projects)
			return
		}

		if cleanPath == "/api/stats" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
				return
			}
			q := parsedURL.Query()
			scope := q.Get("project_path")
			if scope == "" {
				scope = q.Get("projectPath")
			}
			stats, err := store.GetStats(scope)
			if err != nil {
				sendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			sendJSON(w, http.StatusOK, stats)
			return
		}

		if cleanPath == "/api/tasks" {
			if r.Method == http.MethodGet {
				q := parsedURL.Query()
				allBool := false
				if b, ok := parseBoolParam(q.Get("all")); ok && b {
					allBool = true
				}
				search := q.Get("search")
				if search == "" {
					search = q.Get("q")
				}
				projParam := q.Get("project_path")
				if projParam == "" {
					projParam = q.Get("projectPath")
				}
				opts := ListOptions{
					ProjectPath: normalizeAPIProjectPath(projParam),
					Status:      q.Get("status"),
					Priority:    q.Get("priority"),
					Search:      search,
					Sort:        q.Get("sort"),
					All:         allBool,
				}
				tasks, err := store.ListTasks(opts)
				if err != nil {
					if IsValidationError(err) {
						sendError(w, http.StatusBadRequest, err.Error())
						return
					}
					sendError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if tasks == nil {
					tasks = []Task{}
				}
				sendJSON(w, http.StatusOK, tasks)
				return
			}

			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				body, err := readJSONBody(r)
				if err != nil {
					sendError(w, http.StatusBadRequest, err.Error())
					return
				}
				task, err := store.CreateTask(body)
				if err != nil {
					if IsValidationError(err) {
						sendError(w, http.StatusBadRequest, err.Error())
						return
					}
					sendError(w, http.StatusInternalServerError, err.Error())
					return
				}
				sendJSON(w, http.StatusCreated, task)
				return
			}

			w.Header().Set("Allow", "GET, POST, PUT")
			sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
			return
		}

		if strings.HasPrefix(cleanPath, "/api/tasks/") {
			idPart := strings.TrimPrefix(cleanPath, "/api/tasks/")
			id, unescapeErr := url.PathUnescape(idPart)
			if unescapeErr != nil || id == "" || strings.Contains(id, "/") {
				sendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", idPart))
				return
			}

			if r.Method == http.MethodGet {
				task, err := store.GetTask(id)
				if err != nil {
					sendError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if task == nil {
					sendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
					return
				}
				sendJSON(w, http.StatusOK, task)
				return
			}

			if r.Method == http.MethodPatch || r.Method == http.MethodPut {
				body, err := readJSONBody(r)
				if err != nil {
					sendError(w, http.StatusBadRequest, err.Error())
					return
				}
				fields := pickWritable(body)
				if len(fields) == 0 {
					sendError(w, http.StatusBadRequest, "No valid fields provided to update")
					return
				}

				existing, err := store.GetTask(id)
				if err != nil {
					sendError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if existing == nil {
					sendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
					return
				}

				updated, err := store.UpdateTask(id, fields)
				if err != nil {
					if IsValidationError(err) {
						sendError(w, http.StatusBadRequest, err.Error())
						return
					}
					sendError(w, http.StatusInternalServerError, err.Error())
					return
				}
				sendJSON(w, http.StatusOK, updated)
				return
			}

			if r.Method == http.MethodDelete {
				deleted, err := store.DeleteTask(id)
				if err != nil {
					sendError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if !deleted {
					sendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
					return
				}
				sendJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
				return
			}

			w.Header().Set("Allow", "GET, PATCH, PUT, DELETE")
			sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
			return
		}

		if strings.HasPrefix(cleanPath, "/api/") {
			sendError(w, http.StatusNotFound, fmt.Sprintf("Unknown API endpoint: %s", cleanPath))
			return
		}

		// Static UI assets
		serveStaticAsset(w, r, cleanPath, staticFS)
	})
}

func isAddressInUse(err error) bool {
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
	if opts.FS == nil {
		opts.FS = getPublicFS()
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
			if !isAddressInUse(err) || attempt == opts.MaxAttempts {
				return nil, fmt.Errorf("bind to port %d: %w", portToTry, err)
			}
		}
	}

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if ok {
		actualPort = tcpAddr.Port
	}
	actualURL := fmt.Sprintf("http://127.0.0.1:%d", actualPort)

	handler := CreateApp(opts.DB, opts.FS)
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
