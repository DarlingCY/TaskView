package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ServerState represents the on-disk state of a managed background server.
type ServerState struct {
	PID        int    `json:"pid"`
	URL        string `json:"url"`
	DB         string `json:"db"`
	Token      string `json:"token"`
	Instance   string `json:"instance"`
	StartedAt  int64  `json:"started_at"`
	Version    string `json:"version,omitempty"`
	Executable string `json:"executable,omitempty"`
}

// ValidatePort validates that port is in the valid TCP range [0, 65535].
func ValidatePort(port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("invalid port %d: must be between 0 and 65535", port)
	}
	return nil
}

func defaultDaemonCmdBuilder(port int, absDataDir string) (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("locate current executable: %w", err)
	}
	return exe, []string{"--internal-run", "--port", strconv.Itoa(port), "--data-dir", absDataDir}, nil
}

var daemonCmdBuilder = func(port int, absDataDir string) (string, []string, error) {
	return defaultDaemonCmdBuilder(port, absDataDir)
}

func generateRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand read failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func absEqual(p1, p2 string) bool {
	clean1 := filepath.Clean(p1)
	clean2 := filepath.Clean(p2)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(clean1, clean2)
	}
	return clean1 == clean2
}

// acquireProcessLock acquires an OS-held exclusive file lock on filename.
// Returns a release function that releases the lock and closes the file descriptor.
// The lock file is never unlinked to prevent inode races.
func acquireProcessLock(filename string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory for %s: %w", filename, err)
	}

	if timeout <= 0 {
		lock, err := acquireOSLock(filename)
		if err != nil {
			return nil, fmt.Errorf("acquire process lock on %s: %w", filename, err)
		}
		return func() {
			_ = releaseOSLock(lock)
		}, nil
	}

	deadline := time.Now().Add(timeout)
	for {
		lock, err := acquireOSLock(filename)
		if err == nil {
			return func() {
				_ = releaseOSLock(lock)
			}, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %v acquiring process lock on %s: %w", timeout, filename, err)
		}

		time.Sleep(25 * time.Millisecond)
	}
}

func writeServerStateAtomic(dataDir string, state *ServerState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmpName := fmt.Sprintf("server.json.tmp.%s", state.Instance)
	tmpPath := filepath.Join(dataDir, tmpName)
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	finalPath := filepath.Join(dataDir, "server.json")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish server state: %w", err)
	}
	return nil
}

func readServerState(dataDir string) (*ServerState, error) {
	p := filepath.Join(dataDir, "server.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state ServerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid server metadata: %w", err)
	}
	return &state, nil
}

func removeServerStateIfMatching(dataDir string, instance string) {
	current, err := readServerState(dataDir)
	if err == nil && current != nil && current.Instance == instance {
		_ = os.Remove(filepath.Join(dataDir, "server.json"))
	}
}

func validateManagedMetadataURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("empty server URL in metadata")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	if u.Scheme != "http" {
		return nil, errors.New("invalid server scheme: strictly http required")
	}
	if u.User != nil {
		return nil, errors.New("user credentials in server URL forbidden")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("path in server URL forbidden")
	}
	if u.RawQuery != "" {
		return nil, errors.New("query in server URL forbidden")
	}
	if u.Fragment != "" {
		return nil, errors.New("fragment in server URL forbidden")
	}
	hostname := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if hostname != "127.0.0.1" {
		return nil, errors.New("invalid host: strictly 127.0.0.1 required")
	}
	portStr := u.Port()
	if portStr == "" {
		return nil, errors.New("port missing in server URL")
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return nil, errors.New("invalid port in server URL")
	}
	return u, nil
}

func newControlHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil, // Explicitly nil to prevent leaking secrets to proxies
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("redirects rejected on control endpoint")
		},
	}
}

func extractControlToken(r *http.Request) string {
	if t := r.Header.Get("X-Server-Token"); t != "" {
		return strings.TrimSpace(t)
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

type managedStateHolder struct {
	mu    sync.RWMutex
	state ServerState
}

func (h *managedStateHolder) snapshot() ServerState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state
}

func (h *managedStateHolder) setURL(u string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.URL = u
}

func wrapControlHandler(base http.Handler, holder *managedStateHolder, stopCh chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanPath := path.Clean(r.URL.Path)

		if cleanPath == "/api/_server/health" || cleanPath == "/api/_server/stop" {
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
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Server-Token")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			snap := holder.snapshot()

			if cleanPath == "/api/_server/health" {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					w.Header().Set("Allow", "GET, HEAD")
					sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
					return
				}
				token := extractControlToken(r)
				if subtle.ConstantTimeCompare([]byte(token), []byte(snap.Token)) != 1 {
					sendError(w, http.StatusUnauthorized, "Unauthorized")
					return
				}
				// Note: private token is never echoed in health response
				sendJSON(w, http.StatusOK, map[string]any{
					"success":    true,
					"pid":        snap.PID,
					"instance":   snap.Instance,
					"db":         snap.DB,
					"url":        snap.URL,
					"started_at": snap.StartedAt,
					"version":    snap.Version,
					"executable": snap.Executable,
				})
				return
			}

			if cleanPath == "/api/_server/stop" {
				if r.Method != http.MethodPost {
					w.Header().Set("Allow", "POST")
					sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
					return
				}
				token := extractControlToken(r)
				if subtle.ConstantTimeCompare([]byte(token), []byte(snap.Token)) != 1 {
					sendError(w, http.StatusUnauthorized, "Unauthorized")
					return
				}
				sendJSON(w, http.StatusOK, map[string]any{
					"success": true,
					"message": "shutting down",
				})
				go func() {
					time.Sleep(50 * time.Millisecond)
					select {
					case stopCh <- struct{}{}:
					default:
					}
				}()
				return
			}
		}

		base.ServeHTTP(w, r)
	})
}

func checkManagedHealth(state *ServerState, expectedDB string) (bool, *ServerState, error) {
	if state == nil || state.URL == "" || state.Token == "" {
		return false, nil, errors.New("incomplete server state")
	}

	targetURL, err := validateManagedMetadataURL(state.URL)
	if err != nil {
		return false, nil, fmt.Errorf("metadata URL validation failed: %w", err)
	}

	if !absEqual(state.DB, expectedDB) {
		return false, nil, fmt.Errorf("metadata DB %q does not match expected DB %q", state.DB, expectedDB)
	}

	client := newControlHTTPClient(1500 * time.Millisecond)

	expectedEndpoint := fmt.Sprintf("http://127.0.0.1:%s/api/_server/health", targetURL.Port())
	req, err := http.NewRequest(http.MethodGet, expectedEndpoint, nil)
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("X-Server-Token", state.Token)

	resp, err := client.Do(req)
	if err != nil {
		return false, nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, nil, fmt.Errorf("control health check returned status %d", resp.StatusCode)
	}

	var payload struct {
		Success    bool   `json:"success"`
		PID        int    `json:"pid"`
		Instance   string `json:"instance"`
		DB         string `json:"db"`
		URL        string `json:"url"`
		StartedAt  int64  `json:"started_at"`
		Version    string `json:"version,omitempty"`
		Executable string `json:"executable,omitempty"`
	}
	limited := io.LimitReader(resp.Body, 64*1024)
	if err := json.NewDecoder(limited).Decode(&payload); err != nil {
		return false, nil, fmt.Errorf("control health check returned invalid JSON: %w", err)
	}

	expectedBaseURL := fmt.Sprintf("http://127.0.0.1:%s", targetURL.Port())
	if !payload.Success || payload.PID <= 0 || payload.PID != state.PID ||
		payload.Instance != state.Instance || !absEqual(payload.DB, expectedDB) ||
		payload.URL != expectedBaseURL {
		return false, nil, errors.New("control health check returned mismatched identity or URL")
	}

	matchedState := &ServerState{
		PID:        payload.PID,
		URL:        expectedBaseURL,
		DB:         expectedDB,
		Token:      state.Token,
		Instance:   payload.Instance,
		StartedAt:  payload.StartedAt,
		Version:    payload.Version,
		Executable: payload.Executable,
	}
	return true, matchedState, nil
}

// ManagedServerStatus returns the status of a managed background server without side effects.
// Read-only: no mkdir, write, start, or signal. Returns nil, nil when absent or stopped.
func ManagedServerStatus() (*ServerState, error) {
	dataDir, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("locate data directory: %w", err)
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}

	state, err := readServerState(absDataDir)
	if err != nil {
		return nil, fmt.Errorf("read server state: %w", err)
	}
	if state == nil {
		return nil, nil
	}

	dbPath, err := DefaultDBPath()
	if err != nil {
		return nil, fmt.Errorf("locate database path: %w", err)
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}

	if !absEqual(state.DB, absDBPath) {
		return nil, fmt.Errorf("metadata DB %q does not match current project DB %q", state.DB, absDBPath)
	}

	ok, liveState, err := checkManagedHealth(state, absDBPath)
	if err != nil {
		return nil, fmt.Errorf("server metadata invalid or stale: %w", err)
	}
	if !ok {
		return nil, nil
	}

	return liveState, nil
}

// stopBackgroundServerLocked stops the background server and retains both startup.lock
// and server.lock. Callers (such as the installer) must call release() when finished
// with binary operations.
func stopBackgroundServerLocked() (func(), error) {
	dataDir, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("locate data directory: %w", err)
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}

	// If directory absent, return no-op release immediately without mkdir
	if _, err := os.Stat(absDataDir); os.IsNotExist(err) {
		return func() {}, nil
	}

	// 1. Serialize Stop with startup.lock
	startupLockPath := filepath.Join(absDataDir, "startup.lock")
	unlockStartup, err := acquireProcessLock(startupLockPath, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("acquire startup lock for stop: %w", err)
	}

	serverLockPath := filepath.Join(absDataDir, "server.lock")

	makeCombinedRelease := func(unlockServer func()) func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				if unlockServer != nil {
					unlockServer()
				}
				if unlockStartup != nil {
					unlockStartup()
				}
			})
		}
	}

	state, err := readServerState(absDataDir)
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("read server state: %w", err)
	}

	if state == nil {
		if unlockServer, lockErr := acquireProcessLock(serverLockPath, 0); lockErr == nil {
			return makeCombinedRelease(unlockServer), nil
		}
		unlockStartup()
		return nil, fmt.Errorf("server process is running (holds %s) but metadata is absent", serverLockPath)
	}

	dbPath, err := DefaultDBPath()
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("locate database path: %w", err)
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("resolve database path: %w", err)
	}

	if !absEqual(state.DB, absDBPath) {
		unlockStartup()
		return nil, fmt.Errorf("metadata DB %q does not match current project DB %q", state.DB, absDBPath)
	}

	ok, _, err := checkManagedHealth(state, absDBPath)
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("refusing to stop server: %w", err)
	}
	if !ok {
		// Server health check unreachable: test if server.lock is held
		if unlockServer, lockErr := acquireProcessLock(serverLockPath, 0); lockErr == nil {
			removeServerStateIfMatching(absDataDir, state.Instance)
			return makeCombinedRelease(unlockServer), nil
		}
		unlockStartup()
		return nil, fmt.Errorf("server process is unresponsive but still holds lock %s", serverLockPath)
	}

	targetURL, err := validateManagedMetadataURL(state.URL)
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("refusing to stop server with invalid metadata URL: %w", err)
	}

	client := newControlHTTPClient(3 * time.Second)
	endpoint := fmt.Sprintf("http://127.0.0.1:%s/api/_server/stop", targetURL.Port())
	stopReq, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("create stop request: %w", err)
	}
	stopReq.Header.Set("X-Server-Token", state.Token)

	resp, err := client.Do(stopReq)
	if err != nil {
		unlockStartup()
		return nil, fmt.Errorf("send stop request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		unlockStartup()
		return nil, fmt.Errorf("stop request returned status %d", resp.StatusCode)
	}

	// Wait bounded <= 5s to acquire server.lock (proving server process exited and released lock)
	waitDeadline := time.Now().Add(5 * time.Second)
	var finalServerUnlock func()
	for time.Now().Before(waitDeadline) {
		if unlock, err := acquireProcessLock(serverLockPath, 0); err == nil {
			finalServerUnlock = unlock
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalServerUnlock == nil {
		unlockStartup()
		return nil, fmt.Errorf("server at %s acknowledged stop but did not release lock within 5s", state.URL)
	}

	removeServerStateIfMatching(absDataDir, state.Instance)
	return makeCombinedRelease(finalServerUnlock), nil
}

var stopManagedServerForInstall = stopBackgroundServerLocked

// StopBackgroundServer gracefully stops a running background server via authenticated HTTP stop control.
// Absent idempotent. Refuses to stop on instance mismatch. Never issues OS kill by PID.
func StopBackgroundServer() error {
	release, err := stopBackgroundServerLocked()
	if err != nil {
		return err
	}
	if release != nil {
		release()
	}
	return nil
}

// EnsureBackgroundServer ensures a background server is running for the active TASKVIEW_HOME and database.
// Reuses an existing healthy instance; otherwise spawns a detached child server.
func EnsureBackgroundServer(port int) (*ServerState, error) {
	if err := ValidatePort(port); err != nil {
		return nil, err
	}

	dataDir, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("locate data directory: %w", err)
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(absDataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	dbPath, err := DefaultDBPath()
	if err != nil {
		return nil, fmt.Errorf("locate database path: %w", err)
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}

	// 1. Acquire install.lock before startup.lock (lock hierarchy: install -> startup -> server)
	// to prevent starting a new server process or reusing an active server during concurrent install or uninstall.
	installLockPath := filepath.Join(absDataDir, "install.lock")
	unlockInstall, err := acquireProcessLock(installLockPath, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire install lock: %w", err)
	}
	defer unlockInstall()

	// 2. Acquire startup lock to serialize concurrent Ensure callers and ensure any Stop operation has completed
	startupLockPath := filepath.Join(absDataDir, "startup.lock")
	unlockStartup, err := acquireProcessLock(startupLockPath, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire startup lock: %w", err)
	}
	defer unlockStartup()

	// 3. Check health inside both install.lock and startup.lock (reuse only under both locks; no pre-startup fastpath)
	if state, _ := readServerState(absDataDir); state != nil {
		if ok, liveState, _ := checkManagedHealth(state, absDBPath); ok && liveState != nil {
			return liveState, nil
		}
	}

	// 4. Test server.lock to see if a process is already running
	serverLockPath := filepath.Join(absDataDir, "server.lock")
	testUnlockServer, err := acquireProcessLock(serverLockPath, 0)
	if err != nil {
		healthDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(healthDeadline) {
			time.Sleep(100 * time.Millisecond)
			if state, _ := readServerState(absDataDir); state != nil {
				if ok, liveState, _ := checkManagedHealth(state, absDBPath); ok && liveState != nil {
					return liveState, nil
				}
			}
		}
		return nil, fmt.Errorf("server process holds lock but is not responding; check logs at %s", filepath.Join(absDataDir, "server.log"))
	}
	// We acquired server.lock: no server process is running.
	// Release the test lock; do not delete metadata here, RunManagedServer overwrites atomically while holding server.lock.
	testUnlockServer()

	// 5. Spawn background server process
	exe, args, err := daemonCmdBuilder(port, absDataDir)
	if err != nil {
		return nil, fmt.Errorf("build daemon command: %w", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir, _ = os.Getwd()
	cmd.Env = append(os.Environ(), "TASKVIEW_HOME="+absDataDir)

	logFilePath := filepath.Join(absDataDir, "server.log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server.log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	setDetachedProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("failed to spawn server process: %w", err)
	}
	_ = logFile.Close()
	_ = cmd.Process.Release()

	// 6. Wait bounded health <= 10s
	waitDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(waitDeadline) {
		time.Sleep(100 * time.Millisecond)
		state, err := readServerState(absDataDir)
		if err != nil || state == nil {
			continue
		}
		ok, liveState, _ := checkManagedHealth(state, absDBPath)
		if ok && liveState != nil {
			return liveState, nil
		}
	}

	return nil, fmt.Errorf("server failed to start within 10s; check logs at %s", logFilePath)
}

// trayReadyTimeout bounds how long startup waits for the notification area
// icon. It is a portable constant because the tray backends keep their own
// platform specific timeouts.
const trayReadyTimeout = 5 * time.Second

// RunManagedServer is the headless lifecycle used by isolated service tests.
func RunManagedServer(port int) error {
	return runManagedServer(port, nil)
}

func runManagedServer(port int, tray func(context.Context, trayOptions) error) error {
	if err := ValidatePort(port); err != nil {
		return err
	}

	dataDir, err := DataDir()
	if err != nil {
		return fmt.Errorf("locate data directory: %w", err)
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(absDataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	logFilePath := filepath.Join(absDataDir, "server.log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open server.log: %w", err)
	}
	defer logFile.Close()

	logger := log.New(logFile, "[taskview-daemon] ", log.LstdFlags)

	serverLockPath := filepath.Join(absDataDir, "server.lock")
	unlockServer, err := acquireProcessLock(serverLockPath, 0)
	if err != nil {
		logger.Printf("server already running (failed to acquire %s): %v", serverLockPath, err)
		return fmt.Errorf("server already running (failed to acquire %s): %w", serverLockPath, err)
	}
	defer unlockServer()

	token, err := generateRandomHex(32)
	if err != nil {
		logger.Printf("generate control token failed: %v", err)
		return fmt.Errorf("generate control token: %w", err)
	}
	instance, err := generateRandomHex(16)
	if err != nil {
		logger.Printf("generate instance id failed: %v", err)
		return fmt.Errorf("generate instance id: %w", err)
	}
	now := time.Now().UnixMilli()

	dbPath, err := DefaultDBPath()
	if err != nil {
		logger.Printf("locate database path failed: %v", err)
		return fmt.Errorf("locate database path: %w", err)
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		logger.Printf("resolve database path failed: %v", err)
		return fmt.Errorf("resolve database path: %w", err)
	}

	store, err := OpenStore(absDBPath)
	if err != nil {
		logger.Printf("open store failed: %v", err)
		return fmt.Errorf("open store at %s: %w", absDBPath, err)
	}
	defer store.Close()

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate application: %w", err)
	}
	holder := &managedStateHolder{
		state: ServerState{
			PID:        os.Getpid(),
			URL:        "",
			DB:         store.FilePath,
			Token:      token,
			Instance:   instance,
			StartedAt:  now,
			Version:    appVersion(),
			Executable: executable,
		},
	}

	stopCh := make(chan struct{}, 1)
	running, err := StartServer(ServerOptions{
		Port:        port,
		DB:          store,
		OpenBrowser: false,
		WrapHandler: func(base http.Handler) http.Handler {
			return wrapControlHandler(base, holder, stopCh)
		},
	})
	if err != nil {
		logger.Printf("start HTTP server failed: %v", err)
		return fmt.Errorf("start HTTP server: %w", err)
	}
	defer running.Close()

	holder.setURL(running.URL)

	// The tray is started before the state is published: a health check must
	// only succeed for an instance whose notification area icon really exists,
	// otherwise the installer would accept a half-started application that the
	// user cannot reach.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trayReady := make(chan struct{})
	var trayReadyOnce sync.Once
	markTrayReady := func() { trayReadyOnce.Do(func() { close(trayReady) }) }
	var trayDone chan error
	if tray != nil {
		trayDone = make(chan error, 1)
		go func() {
			trayDone <- tray(ctx, trayOptions{
				URL: running.URL, DataDir: absDataDir, Context: ctx,
				Ready: markTrayReady,
				RequestExit: func() {
					select {
					case stopCh <- struct{}{}:
					default:
					}
				},
				CheckUpdate: checkForApplicationUpdate,
			})
		}()
		select {
		case <-trayReady:
		case err := <-trayDone:
			if err != nil {
				return fmt.Errorf("system tray: %w", err)
			}
			return errors.New("system tray stopped before it became ready")
		case <-time.After(trayReadyTimeout):
			cancel()
			select {
			case <-trayDone:
			case <-time.After(2 * time.Second):
			}
			return errors.New("the TaskView tray icon did not start; close the other session or restart Windows Explorer")
		}
	}

	publishedState := holder.snapshot()

	if err := writeServerStateAtomic(absDataDir, &publishedState); err != nil {
		logger.Printf("write server state failed: %v", err)
		return fmt.Errorf("write server state: %w", err)
	}
	defer removeServerStateIfMatching(absDataDir, instance)
	if err := writeEndpoint(absDataDir, &publishedState); err != nil {
		return err
	}
	defer removeEndpoint(absDataDir, instance)

	logger.Printf("managed server listening at %s pid=%d instance=%s db=%s", running.URL, publishedState.PID, publishedState.Instance, publishedState.DB)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	select {
	case sig := <-sigChan:
		logger.Printf("received signal %v, shutting down...", sig)
	case <-stopCh:
		logger.Printf("received control stop request, shutting down...")
	case err := <-trayDone:
		if err != nil {
			return fmt.Errorf("system tray: %w", err)
		}
		trayDone = nil
	}
	cancel()
	if trayDone != nil {
		select {
		case err := <-trayDone:
			if err != nil {
				logger.Printf("tray shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			logger.Printf("tray shutdown timed out")
		}
	}

	logger.Printf("server stopped cleanly")
	return nil
}
