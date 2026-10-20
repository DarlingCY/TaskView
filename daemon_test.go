package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_DAEMON_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}

	port := 0
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			p, err := strconv.Atoi(args[i+1])
			if err == nil {
				port = p
			}
		}
		if args[i] == "--data-dir" && i+1 < len(args) {
			_ = os.Setenv("TASKVIEW_HOME", args[i+1])
		}
	}

	if err := RunManagedServer(port); err != nil {
		fmt.Fprintf(os.Stderr, "RunManagedServer failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func setupDaemonTest(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("TASKVIEW_HOME", tmpHome)
	t.Setenv("GO_WANT_DAEMON_HELPER_PROCESS", "1")

	origBuilder := daemonCmdBuilder
	t.Cleanup(func() {
		daemonCmdBuilder = origBuilder
	})

	daemonCmdBuilder = func(port int, absDataDir string) (string, []string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", nil, err
		}
		args := []string{
			"-test.run=^TestDaemonHelperProcess$",
			"--",
			"server", "run",
			"--port", strconv.Itoa(port),
			"--data-dir", absDataDir,
		}
		return exe, args, nil
	}

	return tmpHome
}

func TestAcquireProcessLock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.lock")

	unlock1, err := acquireProcessLock(lockPath, 0)
	if err != nil {
		t.Fatalf("expected lock acquisition, got: %v", err)
	}

	// Second acquisition with 0 timeout must fail immediately
	_, err = acquireProcessLock(lockPath, 0)
	if err == nil {
		t.Fatalf("expected second lock acquisition to fail")
	}

	// Second acquisition with bounded timeout must timeout
	start := time.Now()
	_, err = acquireProcessLock(lockPath, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("expected timeout after ~100ms, elapsed %v", elapsed)
	}

	// Release first lock
	unlock1()

	// Third acquisition must now succeed
	unlock3, err := acquireProcessLock(lockPath, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("expected lock acquisition after release, got: %v", err)
	}
	unlock3()
}

func TestManagedServerStatusNoSideEffects(t *testing.T) {
	tmpHome := filepath.Join(t.TempDir(), "nonexistent_taskview_dir")
	t.Setenv("TASKVIEW_HOME", tmpHome)

	// Directory does not exist; ManagedServerStatus should not create it
	status, err := ManagedServerStatus()
	if err != nil {
		t.Fatalf("expected nil err, got: %v", err)
	}
	if status != nil {
		t.Fatalf("expected nil status, got: %+v", status)
	}

	if _, err := os.Stat(tmpHome); !os.IsNotExist(err) {
		t.Fatalf("expected data directory not to be created, but it exists")
	}
}

func TestEnsureBackgroundServerAndLifecycle(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	// 1. First ensure call starts background server on ephemeral port 0
	state1, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("EnsureBackgroundServer failed: %v", err)
	}
	if state1 == nil || state1.PID == 0 || state1.Token == "" || state1.Instance == "" {
		t.Fatalf("incomplete state returned: %+v", state1)
	}
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	// Verify server.json exists and contains correct metadata
	savedState, err := readServerState(tmpHome)
	if err != nil || savedState == nil {
		t.Fatalf("failed to read server.json: %v", err)
	}
	if savedState.Instance != state1.Instance || savedState.Token != state1.Token {
		t.Fatalf("saved state mismatch: %+v vs %+v", savedState, state1)
	}

	// 2. Status returns live state
	status, err := ManagedServerStatus()
	if err != nil || status == nil {
		t.Fatalf("ManagedServerStatus failed: %v, status: %+v", err, status)
	}
	if status.Instance != state1.Instance || status.PID != state1.PID {
		t.Fatalf("status mismatch: %+v vs %+v", status, state1)
	}

	// 3. Second ensure call reuses healthy server (same PID and Instance)
	state2, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("EnsureBackgroundServer reuse failed: %v", err)
	}
	if state2.Instance != state1.Instance || state2.PID != state1.PID {
		t.Fatalf("expected reuse of instance %s pid %d, got instance %s pid %d",
			state1.Instance, state1.PID, state2.Instance, state2.PID)
	}

	// 4. Test control endpoint security
	t.Run("control endpoint auth", func(t *testing.T) {
		// Unauthorized without token
		res, err := http.Get(state1.URL + "/api/_server/health")
		if err != nil || res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 without token, got status: %v, err: %v", res, err)
		}
		_ = res.Body.Close()

		// Unauthorized with invalid token
		req, _ := http.NewRequest(http.MethodGet, state1.URL+"/api/_server/health", nil)
		req.Header.Set("X-Server-Token", "invalid_token_123")
		res2, err := http.DefaultClient.Do(req)
		if err != nil || res2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 with bad token, got status: %v", res2)
		}
		_ = res2.Body.Close()

		// Public health check does NOT expose token
		pubRes, err := http.Get(state1.URL + "/api/health")
		if err != nil || pubRes.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on /api/health, got %v", pubRes)
		}
		pubBody := make([]byte, 1024)
		n, _ := pubRes.Body.Read(pubBody)
		_ = pubRes.Body.Close()
		if strings.Contains(string(pubBody[:n]), state1.Token) {
			t.Fatalf("public health endpoint leaked private control token!")
		}
	})

	// 5. Stop server
	if err := StopBackgroundServer(); err != nil {
		t.Fatalf("StopBackgroundServer failed: %v", err)
	}

	// 6. Verify status reports stopped (nil, nil)
	stoppedStatus, err := ManagedServerStatus()
	if err != nil {
		t.Fatalf("expected nil err after stop, got: %v", err)
	}
	if stoppedStatus != nil {
		t.Fatalf("expected nil status after stop, got: %+v", stoppedStatus)
	}

	// 7. Stop is idempotent
	if err := StopBackgroundServer(); err != nil {
		t.Fatalf("idempotent StopBackgroundServer failed: %v", err)
	}
}

func TestStaleMetadataRecovery(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	// Write stale metadata for an unreachable server on port 65432
	stale := &ServerState{
		PID:       999999,
		URL:       "http://127.0.0.1:65432",
		DB:        filepath.Join(tmpHome, "tasks.db"),
		Token:     "stale_token",
		Instance:  "stale_instance",
		StartedAt: time.Now().UnixMilli() - 100000,
	}
	if err := writeServerStateAtomic(tmpHome, stale); err != nil {
		t.Fatalf("write stale state: %v", err)
	}

	// ManagedServerStatus should report stopped (nil, nil) without side effects
	st, err := ManagedServerStatus()
	if err != nil || st != nil {
		t.Fatalf("expected nil, nil for dead server metadata, got %v, err: %v", st, err)
	}

	// EnsureBackgroundServer should detect stale state, replace it, and start a healthy instance
	liveState, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("EnsureBackgroundServer failed to recover from stale metadata: %v", err)
	}
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	if liveState.Instance == "stale_instance" {
		t.Fatalf("expected new instance, got stale instance")
	}

	// Verify it is responding
	status, err := ManagedServerStatus()
	if err != nil || status == nil || status.Instance != liveState.Instance {
		t.Fatalf("expected healthy live state, got %+v, err %v", status, err)
	}
}

func TestConcurrentEnsureConverges(t *testing.T) {
	_ = setupDaemonTest(t)
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	const callers = 4
	var wg sync.WaitGroup
	states := make([]*ServerState, callers)
	errs := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			st, err := EnsureBackgroundServer(0)
			states[idx] = st
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
		if states[i] == nil {
			t.Fatalf("caller %d returned nil state", i)
		}
	}

	firstInstance := states[0].Instance
	firstPID := states[0].PID
	for i := 1; i < callers; i++ {
		if states[i].Instance != firstInstance || states[i].PID != firstPID {
			t.Fatalf("caller %d got instance %s pid %d, expected %s pid %d",
				i, states[i].Instance, states[i].PID, firstInstance, firstPID)
		}
	}
}

func TestValidateManagedMetadataURL(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:3333",
		"http://127.0.0.1:80",
		"http://127.0.0.1:65535",
		"http://127.0.0.1:4000/",
	}
	for _, v := range valid {
		u, err := validateManagedMetadataURL(v)
		if err != nil {
			t.Errorf("expected valid URL %q, got err: %v", v, err)
		}
		if u == nil {
			t.Errorf("expected parsed URL for %q", v)
		}
	}

	invalid := []struct {
		url  string
		desc string
	}{
		{"https://127.0.0.1:3333", "https scheme"},
		{"ftp://127.0.0.1:3333", "ftp scheme"},
		{"http://localhost:3333", "localhost instead of 127.0.0.1"},
		{"http://evil.com:3333", "external domain"},
		{"http://192.168.1.5:3333", "non-loopback IP"},
		{"http://127.0.0.1:3333/api", "subpath forbidden"},
		{"http://127.0.0.1:3333?q=1", "query forbidden"},
		{"http://127.0.0.1:3333#frag", "fragment forbidden"},
		{"http://admin:pass@127.0.0.1:3333", "user info forbidden"},
		{"http://127.0.0.1", "missing port"},
		{"http://127.0.0.1:0", "port 0 invalid for metadata"},
		{"http://127.0.0.1:70000", "port out of range"},
		{"", "empty URL"},
	}
	for _, tc := range invalid {
		_, err := validateManagedMetadataURL(tc.url)
		if err == nil {
			t.Errorf("expected error for %s (%q), got nil", tc.desc, tc.url)
		}
	}
}

func TestControlHTTPClientSecurity(t *testing.T) {
	client := newControlHTTPClient(1 * time.Second)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport")
	}
	if tr.Proxy != nil {
		t.Fatalf("expected Transport.Proxy to be nil to prevent leaking secrets to proxies")
	}
}

func TestCopiedMetadataDifferentDataDir(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	// Simulate copied metadata pointing to an unrelated DB path from another TASKVIEW_HOME
	stale := &ServerState{
		PID:       os.Getpid(),
		URL:       "http://127.0.0.1:3333",
		DB:        filepath.Join(t.TempDir(), "other_unrelated_tasks.db"),
		Token:     "fake_token",
		Instance:  "other_instance",
		StartedAt: time.Now().UnixMilli(),
	}
	if err := writeServerStateAtomic(tmpHome, stale); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	// ManagedServerStatus must reject before request
	_, err := ManagedServerStatus()
	if err == nil || !strings.Contains(err.Error(), "does not match current project DB") {
		t.Fatalf("expected rejection for copied metadata DB mismatch, got: %v", err)
	}

	// StopBackgroundServer must reject before request
	err = StopBackgroundServer()
	if err == nil || !strings.Contains(err.Error(), "does not match current project DB") {
		t.Fatalf("expected Stop rejection for copied metadata DB mismatch, got: %v", err)
	}
}

func TestWrongMetadataPIDDoesNotCauseKill(t *testing.T) {
	tmpHome := setupDaemonTest(t)
	dbPath, _ := DefaultDBPath()
	absDBPath, _ := filepath.Abs(dbPath)

	// State with arbitrary non-existent or foreign PID
	stale := &ServerState{
		PID:       9999999,
		URL:       "http://127.0.0.1:65431",
		DB:        absDBPath,
		Token:     "token_123",
		Instance:  "foreign_instance",
		StartedAt: time.Now().UnixMilli(),
	}
	if err := writeServerStateAtomic(tmpHome, stale); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	// Calling Stop when unreachable should safely report nil or clean up without calling kill on PID
	err := StopBackgroundServer()
	if err != nil {
		t.Fatalf("expected clean stop without process kill, got: %v", err)
	}
}

func TestControlMissingOrBadTokenStop(t *testing.T) {
	_ = setupDaemonTest(t)

	state, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("EnsureBackgroundServer: %v", err)
	}
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	client := newControlHTTPClient(2 * time.Second)

	// 1. Missing token on stop endpoint
	req1, _ := http.NewRequest(http.MethodPost, state.URL+"/api/_server/stop", nil)
	res1, err := client.Do(req1)
	if err != nil || res1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing token on stop, got: %v, err: %v", res1, err)
	}
	_ = res1.Body.Close()

	// 2. Bad token on stop endpoint
	req2, _ := http.NewRequest(http.MethodPost, state.URL+"/api/_server/stop", nil)
	req2.Header.Set("X-Server-Token", "wrong_token_456")
	res2, err := client.Do(req2)
	if err != nil || res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad token on stop, got: %v, err: %v", res2, err)
	}
	_ = res2.Body.Close()
}

func TestStopWaitsLockAndUnreachableLockHeldError(t *testing.T) {
	tmpHome := setupDaemonTest(t)
	dbPath, _ := DefaultDBPath()
	absDBPath, _ := filepath.Abs(dbPath)

	// Simulate server process is unreachable on network, but server.lock is held!
	serverLockPath := filepath.Join(tmpHome, "server.lock")
	unlockServer, err := acquireProcessLock(serverLockPath, 0)
	if err != nil {
		t.Fatalf("acquire server.lock: %v", err)
	}

	stale := &ServerState{
		PID:       os.Getpid(),
		URL:       "http://127.0.0.1:65430",
		DB:        absDBPath,
		Token:     "locked_token",
		Instance:  "locked_instance",
		StartedAt: time.Now().UnixMilli(),
	}
	if err := writeServerStateAtomic(tmpHome, stale); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	// StopBackgroundServer must refuse to claim success and return error because server.lock is still held!
	stopErr := StopBackgroundServer()
	if stopErr == nil || !strings.Contains(stopErr.Error(), "still holds lock") {
		t.Fatalf("expected error that server still holds lock, got: %v", stopErr)
	}

	// Release lock; subsequent stop should now succeed
	unlockServer()
	if err := StopBackgroundServer(); err != nil {
		t.Fatalf("expected success after lock released, got: %v", err)
	}
}

func TestStopAbsentMetadataHeldLockError(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	// Ensure no metadata file exists
	_ = os.Remove(filepath.Join(tmpHome, "server.json"))

	// Simulate lifetime server.lock held by a direct scheduled run during startup
	serverLockPath := filepath.Join(tmpHome, "server.lock")
	unlockServer, err := acquireProcessLock(serverLockPath, 0)
	if err != nil {
		t.Fatalf("acquire server.lock: %v", err)
	}

	// StopBackgroundServer must not falsely report stopped (nil); must error because server.lock is held!
	err = StopBackgroundServer()
	if err == nil || !strings.Contains(err.Error(), "metadata is absent") {
		t.Fatalf("expected error when server.lock held without metadata, got: %v", err)
	}

	// Release lock; now that lock is free and metadata absent, Stop should be idempotent (return nil)
	unlockServer()
	if err := StopBackgroundServer(); err != nil {
		t.Fatalf("expected nil when server.lock free and metadata absent, got: %v", err)
	}
}

func TestEnsureFastpathBlockedByInstallLock(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	// Start a healthy server first
	st, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("initial EnsureBackgroundServer: %v", err)
	}
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	// Acquire install.lock as if installer is in progress
	installLockPath := filepath.Join(tmpHome, "install.lock")
	unlockInstall, err := acquireProcessLock(installLockPath, 0)
	if err != nil {
		t.Fatalf("acquire install.lock: %v", err)
	}

	ensureCh := make(chan *ServerState, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := EnsureBackgroundServer(0)
		ensureCh <- s
		errCh <- err
	}()

	// Even though a healthy server exists, Ensure must NOT return early via fastpath while install.lock is held!
	select {
	case <-ensureCh:
		t.Fatalf("EnsureBackgroundServer returned via fastpath while install.lock was held!")
	case <-time.After(200 * time.Millisecond):
		// Expected: blocked waiting on install.lock
	}

	// Release install.lock; Ensure can now complete
	unlockInstall()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Ensure failed after releasing install.lock: %v", err)
		}
		resState := <-ensureCh
		if resState.Instance != st.Instance {
			t.Fatalf("expected instance %s, got %s", st.Instance, resState.Instance)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Ensure timed out after releasing install.lock")
	}
}

func TestEnsureWaitsOnInstallLock(t *testing.T) {
	tmpHome := setupDaemonTest(t)
	installLockPath := filepath.Join(tmpHome, "install.lock")
	unlockInstall, err := acquireProcessLock(installLockPath, 0)
	if err != nil {
		t.Fatalf("acquire install.lock: %v", err)
	}

	ensureDone := make(chan error, 1)
	go func() {
		_, err := EnsureBackgroundServer(0)
		ensureDone <- err
	}()

	// Ensure should be waiting because install.lock is held
	select {
	case err := <-ensureDone:
		t.Fatalf("EnsureBackgroundServer should have waited on install.lock, but returned early with: %v", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: still waiting!
	}

	// Release install.lock; Ensure should now proceed and succeed
	unlockInstall()
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	select {
	case err := <-ensureDone:
		if err != nil {
			t.Fatalf("EnsureBackgroundServer failed after install.lock released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("EnsureBackgroundServer timed out after install.lock released")
	}
}

func TestHeldReservationRejectsCompetingRunAndEnsureWaitsStartup(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	st, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("EnsureBackgroundServer: %v", err)
	}

	// Stop server with locked helper to retain reservation
	release, err := stopBackgroundServerLocked()
	if err != nil {
		t.Fatalf("stopBackgroundServerLocked: %v", err)
	}
	defer release()

	serverLockPath := filepath.Join(tmpHome, "server.lock")

	// 1. While reservation is held, direct acquireOSLock on server.lock must fail
	lock, err := acquireOSLock(serverLockPath)
	if err == nil {
		_ = releaseOSLock(lock)
		t.Fatalf("expected acquireOSLock to fail while reservation is held")
	}

	// 2. Competing RunManagedServer must fail immediately because server.lock is held
	err = RunManagedServer(0)
	if err == nil || !strings.Contains(err.Error(), "server already running") {
		t.Fatalf("expected RunManagedServer to fail with 'server already running', got: %v", err)
	}

	// 3. EnsureBackgroundServer must be blocked waiting on startup.lock
	ensureCh := make(chan *ServerState, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := EnsureBackgroundServer(0)
		ensureCh <- s
		errCh <- err
	}()

	select {
	case <-ensureCh:
		t.Fatalf("EnsureBackgroundServer returned while stop reservation was held!")
	case <-time.After(200 * time.Millisecond):
		// Expected: blocked on startup.lock
	}

	// 4. Release reservation; now EnsureBackgroundServer proceeds, spawns a new server, and returns it
	release()
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("EnsureBackgroundServer failed after release: %v", err)
		}
		newSt := <-ensureCh
		if newSt.Instance == st.Instance {
			t.Fatalf("expected new instance after restart, got same instance %s", st.Instance)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("EnsureBackgroundServer timed out after reservation release")
	}
}

func TestEnsureWaitsStartupLockEvenIfHealthy(t *testing.T) {
	tmpHome := setupDaemonTest(t)

	st, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("initial EnsureBackgroundServer: %v", err)
	}
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	// Manually acquire startup.lock while server is running healthy
	startupLockPath := filepath.Join(tmpHome, "startup.lock")
	unlockStartup, err := acquireProcessLock(startupLockPath, 0)
	if err != nil {
		t.Fatalf("acquire startup.lock: %v", err)
	}

	ensureCh := make(chan *ServerState, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := EnsureBackgroundServer(0)
		ensureCh <- s
		errCh <- err
	}()

	// Ensure must NOT return the healthy server via pre-startup fastpath while startup.lock is held!
	select {
	case <-ensureCh:
		t.Fatalf("EnsureBackgroundServer returned early while startup.lock was held!")
	case <-time.After(200 * time.Millisecond):
		// Expected: blocked waiting on startup.lock
	}

	// Release startup.lock; Ensure must now return the healthy server
	unlockStartup()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("EnsureBackgroundServer failed after releasing startup.lock: %v", err)
		}
		res := <-ensureCh
		if res.Instance != st.Instance {
			t.Fatalf("expected reused instance %s, got %s", st.Instance, res.Instance)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureBackgroundServer timed out after releasing startup.lock")
	}
}

func TestStopAckAndConcurrentEnsureRestart(t *testing.T) {
	_ = setupDaemonTest(t)

	oldSt, err := EnsureBackgroundServer(0)
	if err != nil {
		t.Fatalf("initial EnsureBackgroundServer: %v", err)
	}

	// Call stopBackgroundServerLocked: it sends stop, gets 200 OK, acquires server.lock, and holds both
	release, err := stopBackgroundServerLocked()
	if err != nil {
		t.Fatalf("stopBackgroundServerLocked: %v", err)
	}

	ensureCh := make(chan *ServerState, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := EnsureBackgroundServer(0)
		ensureCh <- s
		errCh <- err
	}()

	// Ensure must wait because stopBackgroundServerLocked holds startup.lock
	select {
	case <-ensureCh:
		t.Fatalf("EnsureBackgroundServer returned while stop locked!")
	case <-time.After(200 * time.Millisecond):
		// Expected: waiting
	}

	// Release stop locks; Ensure must now detect that the old server is gone, restart, and return new instance
	release()
	t.Cleanup(func() {
		_ = StopBackgroundServer()
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("EnsureBackgroundServer restart failed: %v", err)
		}
		newSt := <-ensureCh
		if newSt.Instance == oldSt.Instance {
			t.Fatalf("expected restarted server with new instance, got old instance %s", oldSt.Instance)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("EnsureBackgroundServer timed out on restart")
	}
}
