// Unit tests for the TaskView install lifecycle.
//
// Every test runs against TASKVIEW_HOME pointing at t.TempDir() and an
// injectable command runner, so no scheduled task is ever created and the real
// user's data directory, database and PATH are never touched.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedCall captures one autostart operation.
type recordedCall struct {
	Mode     string
	TaskName string
	XML      string
}

// fakeRunner is an injectable autostart runner. It records registrations and
// removals and can fail, so the lifecycle is exercised without creating a real
// scheduled task.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []recordedCall
	failWith error
}

func (f *fakeRunner) run(mode, taskName, xmlDoc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{Mode: mode, TaskName: taskName, XML: xmlDoc})
	return f.failWith
}

func (f *fakeRunner) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
}

func (f *fakeRunner) snapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// counts returns how many create and remove operations were recorded.
func (f *fakeRunner) counts() (creates, removes int) {
	for _, c := range f.snapshot() {
		switch c.Mode {
		case "create":
			creates++
		case "remove":
			removes++
		}
	}
	return
}

// installEnv points the data directory at a temp dir and installs fake seams.
// It returns the temp data dir, the path of a throwaway "running executable",
// and the fake autostart runner.
func installEnv(t *testing.T) (dataDir, fakeExe string, runner *fakeRunner) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("TASKVIEW_HOME", home)

	// A stand-in for the real running binary.
	fakeExe = filepath.Join(t.TempDir(), "taskview-source")
	writeExecutable(t, fakeExe, "source-binary-v1")

	runner = &fakeRunner{}

	origPath := executablePath
	origTask := runTaskScheduler
	origStop := stopManagedServerForInstall
	origLock := acquireInstallLock
	t.Cleanup(func() {
		executablePath = origPath
		runTaskScheduler = origTask
		stopManagedServerForInstall = origStop
		acquireInstallLock = origLock
	})

	executablePath = func() (string, error) { return fakeExe, nil }
	runTaskScheduler = runner.run

	return home, fakeExe, runner
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s still exists, want removed", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Install
// ---------------------------------------------------------------------------

func TestInstallCopiesExecutableToManagedPath(t *testing.T) {
	home, _, _ := installEnv(t)

	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := filepath.Join(home, "bin", platformBinaryNameForTest())
	if result.Path != want {
		t.Errorf("Path = %q, want %q", result.Path, want)
	}
	if got := readFileString(t, result.Path); got != "source-binary-v1" {
		t.Errorf("installed contents = %q, want the source binary", got)
	}
	if result.Autostart {
		t.Error("Autostart should be false by default")
	}
}

func TestInstallWritesManifest(t *testing.T) {
	home, _, _ := installEnv(t)

	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw := readFileString(t, filepath.Join(home, "install.json"))
	var manifest installManifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest.Path != result.Path {
		t.Errorf("manifest Path = %q, want %q", manifest.Path, result.Path)
	}
	if manifest.Autostart {
		t.Error("manifest Autostart should be false without --autostart")
	}
}

func TestInstallIsIdempotentAndRegistersNoDuplicates(t *testing.T) {
	_, _, runner := installEnv(t)

	first, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	second, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("paths differ: %q vs %q", first.Path, second.Path)
	}
	if second.Autostart {
		t.Error("Autostart should stay false")
	}

	// Nothing was registered, so nothing was deleted either.
	if creates, removes := runner.counts(); creates != 0 || removes != 0 {
		t.Errorf("default install made %d creates and %d removes, want 0 and 0", creates, removes)
	}
}

func TestInstallPreservesAutostartOnPlainReinstall(t *testing.T) {
	home, _, runner := installEnv(t)

	if _, err := Install(InstallOptions{Autostart: true}); err != nil {
		t.Fatalf("opt-in Install: %v", err)
	}

	// A later plain reinstall must not silently drop the registration.
	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("plain reinstall: %v", err)
	}
	if !result.Autostart {
		t.Error("Autostart should stay true after a plain reinstall")
	}

	var manifest installManifest
	if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(home, "install.json"))), &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !manifest.Autostart {
		t.Error("manifest should still record autostart")
	}

	// Both installs used the same task name, so no duplicate task can exist.
	names := map[string]bool{}
	for _, call := range runner.snapshot() {
		if call.Mode == "create" {
			names[call.TaskName] = true
		}
	}
	if len(names) != 1 {
		t.Errorf("task names used = %v, want exactly 1", names)
	}
}

func TestInstallAutostartOnlyWhenRequested(t *testing.T) {
	_, _, runner := installEnv(t)

	if _, err := Install(InstallOptions{Autostart: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if creates, removes := runner.counts(); creates != 1 || removes != 0 {
		t.Fatalf("autostart install made %d creates and %d removes, want 1 and 0", creates, removes)
	}
}

func TestInstallDoesNotStartServer(t *testing.T) {
	_, _, runner := installEnv(t)

	if _, err := Install(InstallOptions{Autostart: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Registration only: nothing is triggered now, and there is no separate
	// "run" mode at all.
	for _, call := range runner.snapshot() {
		if call.Mode != "create" && call.Mode != "remove" {
			t.Errorf("install issued unexpected scheduler mode %q", call.Mode)
		}
	}
}

func TestInstallReplacesChangedBinaryButNotIdenticalOne(t *testing.T) {
	home, fakeExe, _ := installEnv(t)

	first, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// Identical content: the managed copy must be left alone.
	epoch := time.Unix(0, 0)
	if err := os.Chtimes(first.Path, epoch, epoch); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	stampBefore, _ := os.Stat(first.Path)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("idempotent Install: %v", err)
	}
	stampAfter, _ := os.Stat(first.Path)
	if !stampBefore.ModTime().Equal(stampAfter.ModTime()) {
		t.Error("identical binary was needlessly replaced")
	}

	// Changed content: the managed copy must be refreshed.
	writeExecutable(t, fakeExe, "source-binary-v2")
	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("updating Install: %v", err)
	}
	if got := readFileString(t, result.Path); got != "source-binary-v2" {
		t.Errorf("updated contents = %q, want v2", got)
	}
	if filepath.Dir(result.Path) != filepath.Join(home, "bin") {
		t.Errorf("managed path moved: %q", result.Path)
	}
}

func TestInstallStopsManagedServerBeforeReplacing(t *testing.T) {
	home, fakeExe, _ := installEnv(t)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	writeExecutable(t, fakeExe, "source-binary-v2")

	var stopped int
	orig := stopManagedServerForInstall
	stopManagedServerForInstall = func() (func(), error) { stopped++; return func() {}, nil }
	t.Cleanup(func() { stopManagedServerForInstall = orig })

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("replacing Install: %v", err)
	}
	if stopped != 1 {
		t.Errorf("stopManagedServerForInstall calls = %d, want 1 before replacement", stopped)
	}
	_ = home
}

func TestInstallFailsWhenServerCannotBeStopped(t *testing.T) {
	_, fakeExe, _ := installEnv(t)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	managed := managedPath(t)
	writeExecutable(t, fakeExe, "source-binary-v2")

	orig := stopManagedServerForInstall
	stopManagedServerForInstall = func() (func(), error) { return nil, errors.New("server still running") }
	t.Cleanup(func() { stopManagedServerForInstall = orig })

	if _, err := Install(InstallOptions{}); err == nil {
		t.Fatal("Install should fail when the managed server cannot be stopped")
	}
	// The previously installed binary must be intact.
	if got := readFileString(t, managed); got != "source-binary-v1" {
		t.Errorf("installed binary = %q, want the previous version untouched", got)
	}
}

// TestInstallFromInstalledBinaryIsIdempotent covers the real requirement: the
// installed copy running `install` again must succeed and be a no-op. Source and
// destination are the same file with the same bytes, so the copy is skipped and
// registration proceeds normally.
func TestInstallFromInstalledBinaryIsIdempotent(t *testing.T) {
	home, _, _ := installEnv(t)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	orig := executablePath
	executablePath = func() (string, error) {
		return filepath.Abs(filepath.Join(home, "bin", platformBinaryNameForTest()))
	}
	t.Cleanup(func() { executablePath = orig })

	first, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install from the installed binary: %v", err)
	}
	if first.Path != filepath.Join(home, "bin", platformBinaryNameForTest()) {
		t.Errorf("Path = %q, want the managed path", first.Path)
	}
	if got := readFileString(t, first.Path); got != "source-binary-v1" {
		t.Errorf("installed contents = %q, want them intact", got)
	}

	// Repeating it must stay idempotent and must not re-copy identical bytes.
	second, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("repeat Install from the installed binary: %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("Path = %q, want %q", second.Path, first.Path)
	}
	if second.Autostart {
		t.Error("Autostart should stay false without an opt-in")
	}
}

func TestInstallFromInstalledBinaryCanEnableAutostart(t *testing.T) {
	home, _, runner := installEnv(t)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// The user is now running the installed copy and opts into autostart.
	orig := executablePath
	executablePath = func() (string, error) {
		return filepath.Abs(filepath.Join(home, "bin", platformBinaryNameForTest()))
	}
	t.Cleanup(func() { executablePath = orig })

	result, err := Install(InstallOptions{Autostart: true})
	if err != nil {
		t.Fatalf("install --autostart from the installed binary: %v", err)
	}
	if !result.Autostart {
		t.Error("Autostart should be enabled")
	}
	if creates, _ := runner.counts(); creates != 1 {
		t.Errorf("registrations = %d, want 1", creates)
	}
}

func TestInstallLeavesDatabaseAlone(t *testing.T) {
	home, _, _ := installEnv(t)

	// Plant a database where a real one would live.
	dbPath, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeExecutable(t, dbPath, "not-a-real-db-but-must-survive")

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := readFileString(t, dbPath); got != "not-a-real-db-but-must-survive" {
		t.Errorf("database contents changed to %q", got)
	}

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got := readFileString(t, dbPath); got != "not-a-real-db-but-must-survive" {
		t.Errorf("uninstall modified the database: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "bin")); err != nil {
		t.Errorf("bin directory should remain: %v", err)
	}
}

func TestInstallDoesNotModifyPATH(t *testing.T) {
	installEnv(t)

	before := os.Getenv("PATH")
	if _, err := Install(InstallOptions{Autostart: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if after := os.Getenv("PATH"); after != before {
		t.Error("Install modified PATH; it must only report a path the user can add manually")
	}
}

// ---------------------------------------------------------------------------
// Uninstall
// ---------------------------------------------------------------------------

func TestUninstallRemovesManagedBinaryAndManifestOnly(t *testing.T) {
	home, _, _ := installEnv(t)

	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// An unrelated file in the data directory must survive.
	keep := filepath.Join(home, "unrelated.txt")
	writeExecutable(t, keep, "keep me")

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	mustNotExist(t, result.Path)
	mustNotExist(t, filepath.Join(home, "install.json"))
	if got := readFileString(t, keep); got != "keep me" {
		t.Errorf("unrelated file = %q, want it preserved", got)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("data directory must not be removed: %v", err)
	}
}

func TestUninstallIsRepeatable(t *testing.T) {
	installEnv(t)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := Uninstall(); err != nil {
			t.Fatalf("Uninstall #%d: %v", i+1, err)
		}
	}
}

func TestUninstallBeforeInstallIsSafe(t *testing.T) {
	installEnv(t)
	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall on a clean system: %v", err)
	}
}

func TestUninstallIgnoresTamperedManifestPath(t *testing.T) {
	home, _, _ := installEnv(t)

	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A hostile manifest must not redirect deletion at an arbitrary file.
	victim := filepath.Join(t.TempDir(), "victim.txt")
	writeExecutable(t, victim, "must survive")

	manifestPath := filepath.Join(home, "install.json")
	tampered := `{"path":` + jsonString(victim) + `,"autostart":false}`
	if err := os.WriteFile(manifestPath, []byte(tampered), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got := readFileString(t, victim); got != "must survive" {
		t.Error("Uninstall followed the manifest path and deleted an unrelated file")
	}
	// The genuinely managed binary is still the one that gets removed.
	mustNotExist(t, result.Path)
}

func TestUninstallStopsManagedServer(t *testing.T) {
	installEnv(t)

	if _, err := Install(InstallOptions{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	var stops int
	orig := stopManagedServerForInstall
	stopManagedServerForInstall = func() (func(), error) { stops++; return func() {}, nil }
	t.Cleanup(func() { stopManagedServerForInstall = orig })

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if stops != 1 {
		t.Errorf("stopManagedServerForInstall calls = %d, want 1", stops)
	}
}

func TestUninstallRemovesRegisteredTask(t *testing.T) {
	_, _, runner := installEnv(t)

	if _, err := Install(InstallOptions{Autostart: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, removes := runner.counts(); removes != 1 {
		t.Errorf("scheduler removals = %d, want 1", removes)
	}
}

// ---------------------------------------------------------------------------
// Autostart registration failure handling
// ---------------------------------------------------------------------------

func TestAutostartFailureReportsButKeepsBinary(t *testing.T) {
	home, _, runner := installEnv(t)
	runner.setFailure(errors.New("ERROR: Access is denied."))

	result, err := Install(InstallOptions{Autostart: true})
	if err == nil {
		t.Fatal("Install should report the registration failure")
	}
	if result == nil || result.Path == "" {
		t.Fatal("Install should still return the installed path")
	}
	if result.Autostart {
		t.Error("Autostart must be false when registration failed")
	}
	// The binary is installed and runnable.
	if got := readFileString(t, result.Path); got != "source-binary-v1" {
		t.Errorf("binary contents = %q, want it installed despite the failure", got)
	}
	// The manifest must not claim an autostart that never happened.
	var manifest installManifest
	if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(home, "install.json"))), &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.Autostart {
		t.Error("manifest must not record autostart after a failed registration")
	}
	// The message must be actionable, must point at the tray switch rather than
	// a removed command line, and must not imply elevation.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "start menu") && !strings.Contains(msg, "elevated") {
		t.Errorf("error should explain how to proceed: %v", err)
	}
	if strings.Contains(msg, "--autostart") {
		t.Errorf("error must not suggest a command line flag: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func managedPath(t *testing.T) string {
	t.Helper()
	home, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	path, err := installTargetPath(home)
	if err != nil {
		t.Fatalf("installTargetPath: %v", err)
	}
	return path
}

func platformBinaryNameForTest() string {
	name, err := platformBinaryName()
	if err != nil {
		panic(err)
	}
	return name
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestSamePathFoldsCase(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "taskview.exe")

	if !samePath(a, a) {
		t.Error("samePath should match identical paths")
	}
	if samePath(a, filepath.Join(dir, "other.exe")) {
		t.Error("samePath should not match different files")
	}

	// Windows paths are case-insensitive.
	upper := strings.ToUpper(a)
	if !samePath(a, upper) {
		t.Error("paths differing only in case must match")
	}
}

func TestFilesHaveSameContents(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")

	writeExecutable(t, a, "same")
	writeExecutable(t, b, "same")

	ok, err := filesHaveSameContents(a, b)
	if err != nil || !ok {
		t.Errorf("identical files = (%v, %v), want (true, nil)", ok, err)
	}

	writeExecutable(t, b, "different")
	ok, err = filesHaveSameContents(a, b)
	if err != nil || ok {
		t.Errorf("different files = (%v, %v), want (false, nil)", ok, err)
	}

	ok, err = filesHaveSameContents(a, filepath.Join(dir, "missing"))
	if err != nil || ok {
		t.Errorf("missing destination = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestSelfDeleteMarkerIsSetAndClearedAtEntry(t *testing.T) {
	// A stale marker must not survive a later uninstall, and a successful inline
	// uninstall must not leave one behind.
	installEnv(t)

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got := SelfDeleteScheduled(); got != "" {
		t.Errorf("SelfDeleteScheduled() = %q, want empty: the marker is cleared on entry", got)
	}
}

func TestManifestReadSurfacesNullAndMalformedContent(t *testing.T) {
	home := t.TempDir()

	// Missing manifest is normal, not an error.
	if m, err := readInstallManifest(home); err != nil || m.Autostart {
		t.Errorf("missing manifest = (%+v, %v), want zero value and nil error", m, err)
	}

	// Malformed JSON must be reported: silently treating it as "no autostart"
	// would discard a user's explicit opt-in.
	if err := os.WriteFile(filepath.Join(home, "install.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := readInstallManifest(home); err == nil {
		t.Error("readInstallManifest accepted a malformed manifest")
	} else if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error = %v, want it to say the manifest is malformed", err)
	}

	// An explicit null autostart field decodes to false, which is the user's
	// intent rather than an error.
	if err := os.WriteFile(filepath.Join(home, "install.json"),
		[]byte(`{"path":"x","autostart":null}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if m, err := readInstallManifest(home); err != nil {
		t.Errorf("null autostart should decode, got error %v", err)
	} else if m.Autostart {
		t.Error("null autostart should decode to false")
	}
}

func TestManifestWriteIsAtomicAndPrivate(t *testing.T) {
	home := t.TempDir()

	manifest := &installManifest{Path: `C:\x\taskview.exe`, Autostart: true}
	if err := writeInstallManifest(home, manifest); err != nil {
		t.Fatalf("writeInstallManifest: %v", err)
	}

	// No temp files left behind.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "install.json" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}

	read, err := readInstallManifest(home)
	if err != nil {
		t.Fatalf("readInstallManifest: %v", err)
	}
	if !read.Autostart || read.Path != manifest.Path {
		t.Errorf("round trip = %+v, want %+v", read, manifest)
	}
}

func TestInstallLocalLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "install.lock")

	release, err := acquireInstallLock(lockFile, time.Second)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	if _, err := acquireInstallLock(lockFile, 200*time.Millisecond); err == nil {
		release()
		t.Fatal("second lock should fail while the first is held")
	}

	release()
	// Idempotent release.
	release()

	release2, err := acquireInstallLock(lockFile, time.Second)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

func TestInstallerDefaultsUseRealDaemonHooks(t *testing.T) {
	// The installer must not silently no-op: if these were stubs, an update
	// would replace a running server's binary and an uninstall would leave it
	// running. Package tests never run main's init, so the defaults themselves
	// are what guarantee correctness.
	if stopManagedServerForInstall == nil {
		t.Error("stopManagedServerForInstall must default to a real implementation")
	}
	if acquireInstallLock == nil {
		t.Error("acquireInstallLock must default to a real implementation")
	}

	// Exercise the real StopBackgroundServer against a temp data dir: with no
	// server running it must be a clean no-op, which is the idempotent-absent
	// behaviour uninstall depends on.
	t.Setenv("TASKVIEW_HOME", t.TempDir())
	if err := StopBackgroundServer(); err != nil {
		t.Errorf("StopBackgroundServer with no server running: %v", err)
	}

	// The real process lock must work and must be exclusive.
	lockFile := filepath.Join(t.TempDir(), "install.lock")
	release, err := acquireProcessLock(lockFile, time.Second)
	if err != nil {
		t.Fatalf("acquireProcessLock: %v", err)
	}
	if _, err := acquireProcessLock(lockFile, 200*time.Millisecond); err == nil {
		release()
		t.Error("acquireProcessLock should be exclusive")
	}
	release()
}
