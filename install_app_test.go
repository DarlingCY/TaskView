package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The installer coordination is exercised in a private world.
//
// The process temporary directory (the transaction has to live there), the
// data directory and the installation target are all redirected into one test
// directory, and every side effect that would reach the operating system - the
// Task Scheduler, the running server, the lock files - is replaced by a
// recording fake. No real task is created, no server is started and no user
// data is touched.
// ---------------------------------------------------------------------------

type installTestEnv struct {
	root    string
	dataDir string
	target  string
	txFile  string
	txDir   string
}

func newInstallTestEnv(t *testing.T) *installTestEnv {
	t.Helper()
	root := t.TempDir()
	// The installer transaction has to live in a temporary directory.
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	// The installed application path is derived from the per-user cache
	// directory, so redirecting it keeps the test away from the real one.
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "LocalAppData"))

	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	t.Setenv("TASKVIEW_HOME", dataDir)

	target, err := installedApplicationPath()
	if err != nil {
		t.Fatalf("installed application path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("create installation directory: %v", err)
	}

	txDir := filepath.Join(root, "taskview-install-test")
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		t.Fatalf("create transaction directory: %v", err)
	}
	return &installTestEnv{
		root:    root,
		dataDir: dataDir,
		target:  target,
		txFile:  filepath.Join(root, "taskview-transaction.json"),
		txDir:   txDir,
	}
}

func (e *installTestEnv) writeTransaction(t *testing.T, tx installTransaction) {
	t.Helper()
	if err := writeJSONAtomic(e.txFile, tx); err != nil {
		t.Fatalf("write transaction: %v", err)
	}
}

func (e *installTestEnv) writeDecision(t *testing.T, decision installDecision) {
	t.Helper()
	if err := writeJSONAtomic(e.txFile+".decision", decision); err != nil {
		t.Fatalf("write decision: %v", err)
	}
}

// preparePreviousInstallation writes the state of an earlier installation of
// the given version: the executable, the manifest and nothing else. It returns
// the manifest bytes, which are what a recovery has to put back.
func (e *installTestEnv) preparePreviousInstallation(t *testing.T, version string) []byte {
	t.Helper()
	manifest := installManifest{Path: e.target, Autostart: true, InstalledAt: "2026-01-01T00:00:00Z", Version: version}
	if err := writeInstallManifest(e.dataDir, &manifest); err != nil {
		t.Fatalf("write previous manifest: %v", err)
	}
	raw, err := os.ReadFile(installManifestPath(e.dataDir))
	if err != nil {
		t.Fatalf("read previous manifest: %v", err)
	}
	writeExecutable(t, e.target, "previous executable")
	return raw
}

// installFakes records everything the installer coordination asks the
// operating system to do.
type installFakes struct {
	registration autostartRegistration
	healthErr    error
	startErr     error
	schedulerErr error

	started     []string
	healthCalls []string
	stops       int
	scheduler   []string
	restored    []string

	// onStart runs when the application is started, so a test can simulate
	// what the installer does to the executable while the worker waits.
	onStart func(exe, dir string)
}

func installFakeSeams(t *testing.T, f *installFakes) {
	t.Helper()
	origStart, origHealth := installationStart, installationHealth
	origRegistration := installationRegistration
	origStop := stopManagedServerForInstall
	origLock := acquireInstallLock
	origScheduler := runTaskScheduler
	origRestorer := autostartRestorer
	t.Cleanup(func() {
		installationStart, installationHealth = origStart, origHealth
		installationRegistration = origRegistration
		stopManagedServerForInstall = origStop
		acquireInstallLock = origLock
		runTaskScheduler = origScheduler
		autostartRestorer = origRestorer
	})
	installationStart = func(exe, dir string) error {
		f.started = append(f.started, exe)
		if f.onStart != nil {
			f.onStart(exe, dir)
		}
		return f.startErr
	}
	installationHealth = func(ctx context.Context, expected, executable string) error {
		f.healthCalls = append(f.healthCalls, expected+"|"+executable)
		return f.healthErr
	}
	installationRegistration = func(dir string) (autostartRegistration, error) {
		return f.registration, nil
	}
	stopManagedServerForInstall = func() (func(), error) {
		f.stops++
		return func() {}, nil
	}
	acquireInstallLock = func(filename string, timeout time.Duration) (func(), error) {
		return func() {}, nil
	}
	runTaskScheduler = func(mode, taskName, xmlDoc string) error {
		f.scheduler = append(f.scheduler, mode+"|"+taskName)
		return f.schedulerErr
	}
	autostartRestorer = func(dataDir, definition string) error {
		f.restored = append(f.restored, definition)
		return nil
	}
}

// setTestVersion replaces the build version for the duration of a test. The
// version decides whether an installation is an upgrade, a same version
// reinstall or a downgrade.
func setTestVersion(t *testing.T, value string) {
	t.Helper()
	original := version
	version = value
	t.Cleanup(func() { version = original })
}

// ---------------------------------------------------------------------------
// The legacy gate
// ---------------------------------------------------------------------------

func TestValidatePreviousInstallationGatesLegacyAndVersions(t *testing.T) {
	env := newInstallTestEnv(t)
	legacy, err := installTargetPath(env.dataDir)
	if err != nil {
		t.Fatalf("legacy target: %v", err)
	}
	legacyReg := autostartRegistration{Exists: true, Enabled: true, MatchesDataDir: true, Exe: legacy, XML: "<Task>legacy</Task>"}

	var exitErr *applicationExitError
	err = validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: legacy}, legacyReg, false)
	if !errors.As(err, &exitErr) || exitErr.Code != legacyMigrationExitCode {
		t.Fatalf("legacy manifest without confirmation: got %v, want exit code %d", err, legacyMigrationExitCode)
	}
	if err := validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: legacy}, legacyReg, true); err != nil {
		t.Fatalf("legacy manifest with confirmation: %v", err)
	}
	err = validatePreviousInstallation(env.dataDir, env.target, installManifest{}, legacyReg, false)
	if !errors.As(err, &exitErr) || exitErr.Code != legacyMigrationExitCode {
		t.Fatalf("legacy logon task without confirmation: got %v, want exit code %d", err, legacyMigrationExitCode)
	}
	if err := validatePreviousInstallation(env.dataDir, env.target, installManifest{}, legacyReg, true); err != nil {
		t.Fatalf("legacy logon task with confirmation: %v", err)
	}

	// A manifest or a task that belongs to something else is refused
	// outright, with or without the legacy confirmation.
	err = validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: filepath.Join(env.dataDir, "other", "taskview.exe")}, autostartRegistration{}, false)
	if err == nil || !strings.Contains(err.Error(), "unrecognized TaskView installation manifest") {
		t.Fatalf("foreign manifest: got %v, want an unrecognized manifest error", err)
	}
	err = validatePreviousInstallation(env.dataDir, env.target, installManifest{}, autostartRegistration{Exists: true, Enabled: true, Exe: filepath.Join(env.dataDir, "other.exe"), XML: "<Task/>"}, false)
	if err == nil || !strings.Contains(err.Error(), "unrecognized action") {
		t.Fatalf("foreign logon task: got %v, want an unrecognized task error", err)
	}

	// Version rules for an installation that already targets the fixed path.
	setTestVersion(t, "1.2.3")
	if err := validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: env.target, Version: "1.2.3"}, autostartRegistration{}, false); err != nil {
		t.Fatalf("same version reinstall: %v", err)
	}
	if err := validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: env.target, Version: "1.2.2"}, autostartRegistration{}, false); err != nil {
		t.Fatalf("upgrade to a newer version: %v", err)
	}
	err = validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: env.target, Version: "1.2.4"}, autostartRegistration{}, false)
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade: got %v, want a downgrade refusal", err)
	}
	setTestVersion(t, "dev")
	err = validatePreviousInstallation(env.dataDir, env.target, installManifest{Path: env.target, Version: "1.2.3"}, autostartRegistration{}, false)
	if err == nil || !strings.Contains(err.Error(), "unversioned build") {
		t.Fatalf("unversioned build over a release: got %v, want a refusal", err)
	}
}

// ---------------------------------------------------------------------------
// The worker
// ---------------------------------------------------------------------------

func TestApplicationInstallWorkerCommits(t *testing.T) {
	env := newInstallTestEnv(t)
	setTestVersion(t, "1.2.3")
	previousManifest := env.preparePreviousInstallation(t, "1.2.3")

	f := &installFakes{registration: autostartRegistration{Exists: true, Enabled: true, MatchesDataDir: true, Exe: env.target, XML: "<Task>previous</Task>"}}
	installFakeSeams(t, f)

	env.writeTransaction(t, installTransaction{Target: env.target, DataDir: env.dataDir, Version: "1.2.3", Directory: env.txDir, Phase: "starting"})
	env.writeDecision(t, installDecision{Action: "commit", Autostart: "preserve", Start: "no"})

	if err := applicationInstallWorker(env.txFile); err != nil {
		t.Fatalf("commit worker: %v", err)
	}
	tx, err := readInstallTransaction(env.txFile)
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if tx.Phase != "complete" {
		t.Fatalf("transaction phase = %q, want complete", tx.Phase)
	}
	manifest, err := readInstallManifest(env.dataDir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !samePath(manifest.Path, env.target) || manifest.Version != "1.2.3" || !manifest.Autostart {
		t.Fatalf("manifest = %+v, want it to describe the new installation", manifest)
	}
	if got := readFileString(t, installManifestPath(env.dataDir)); got == string(previousManifest) {
		t.Fatalf("the commit left the previous manifest in place: %s", got)
	}
	// The existing registration was handed to the scheduler.
	if len(f.scheduler) != 1 || !strings.HasPrefix(f.scheduler[0], "create|") {
		t.Fatalf("scheduler calls = %v, want a single registration", f.scheduler)
	}
	// The application was not started: nobody asked for it.
	if len(f.started) != 0 || len(f.healthCalls) != 0 {
		t.Fatalf("the worker started the application without being asked to: %v %v", f.started, f.healthCalls)
	}
}

func TestApplicationInstallWorkerFailureRestoresThePreviousInstallation(t *testing.T) {
	env := newInstallTestEnv(t)
	setTestVersion(t, "1.2.3")
	previousManifest := env.preparePreviousInstallation(t, "1.2.3")

	f := &installFakes{
		registration: autostartRegistration{Exists: true, Enabled: true, MatchesDataDir: true, Exe: env.target, XML: "<Task>previous</Task>"},
		healthErr:    errors.New("the application did not report the expected version"),
	}
	// The installer replaces the executable while the worker waits. A new
	// executable that never becomes healthy has to give way to the old one.
	f.onStart = func(exe, dir string) {
		writeExecutable(t, exe, "new executable")
	}
	installFakeSeams(t, f)

	env.writeTransaction(t, installTransaction{Target: env.target, DataDir: env.dataDir, Version: "1.2.3", Directory: env.txDir, Phase: "starting"})
	env.writeDecision(t, installDecision{Action: "commit", Autostart: "preserve", Start: "yes"})

	err := applicationInstallWorker(env.txFile)
	if err == nil {
		t.Fatal("the worker reported success although the application never became healthy")
	}
	if !strings.Contains(err.Error(), "upgrade failed") {
		t.Fatalf("worker error = %v, want an upgrade failure", err)
	}
	if got := readFileString(t, env.target); got != "previous executable" {
		t.Fatalf("executable = %q, want the previous version to be restored", got)
	}
	if got := readFileString(t, installManifestPath(env.dataDir)); got != string(previousManifest) {
		t.Fatalf("manifest = %q, want the previous manifest to be restored", got)
	}
	if len(f.restored) != 1 || f.restored[0] != "<Task>previous</Task>" {
		t.Fatalf("restored registrations = %v, want the previous task definition", f.restored)
	}
	tx, err := readInstallTransaction(env.txFile)
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if tx.Phase != "error" || !strings.Contains(tx.Error, "upgrade failed") {
		t.Fatalf("transaction = %+v, want the failure to be recorded", tx)
	}
}

func TestApplicationInstallWorkerAbortRestoresThePreviousInstallation(t *testing.T) {
	env := newInstallTestEnv(t)
	setTestVersion(t, "1.2.3")
	previousManifest := env.preparePreviousInstallation(t, "1.2.3")

	f := &installFakes{registration: autostartRegistration{Exists: true, Enabled: true, MatchesDataDir: true, Exe: env.target, XML: "<Task>previous</Task>"}}
	installFakeSeams(t, f)

	env.writeTransaction(t, installTransaction{Target: env.target, DataDir: env.dataDir, Version: "1.2.3", Directory: env.txDir, Phase: "starting"})
	env.writeDecision(t, installDecision{Action: "abort"})

	if err := applicationInstallWorker(env.txFile); err != nil {
		t.Fatalf("abort worker: %v", err)
	}
	tx, err := readInstallTransaction(env.txFile)
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if tx.Phase != "aborted" {
		t.Fatalf("transaction phase = %q, want aborted", tx.Phase)
	}
	if got := readFileString(t, env.target); got != "previous executable" {
		t.Fatalf("executable = %q, want the previous version to be left in place", got)
	}
	if got := readFileString(t, installManifestPath(env.dataDir)); got != string(previousManifest) {
		t.Fatalf("manifest = %q, want the previous manifest to be left in place", got)
	}
	if len(f.started) != 0 {
		t.Fatalf("an aborted installation started the application: %v", f.started)
	}
	if len(f.restored) != 1 || f.restored[0] != "<Task>previous</Task>" {
		t.Fatalf("restored registrations = %v, want the previous task definition", f.restored)
	}
	if len(f.scheduler) != 0 {
		t.Fatalf("scheduler calls = %v, want none for an aborted installation", f.scheduler)
	}
}

// The coordinator refuses a legacy installation and a leftover transaction
// before it writes anything or starts a worker.
func TestPrepareApplicationInstallRefusesBeforeItStartsAnything(t *testing.T) {
	env := newInstallTestEnv(t)
	legacy, err := installTargetPath(env.dataDir)
	if err != nil {
		t.Fatalf("legacy target: %v", err)
	}
	if err := writeInstallManifest(env.dataDir, &installManifest{Path: legacy, Version: "0.9.0"}); err != nil {
		t.Fatalf("write legacy manifest: %v", err)
	}
	installFakeSeams(t, &installFakes{registration: autostartRegistration{}})

	var exitErr *applicationExitError
	err = prepareApplicationInstall(env.target, env.txFile, false)
	if !errors.As(err, &exitErr) || exitErr.Code != legacyMigrationExitCode {
		t.Fatalf("prepare with a legacy installation: got %v, want exit code %d", err, legacyMigrationExitCode)
	}
	if _, err := os.Stat(env.txFile); !os.IsNotExist(err) {
		t.Fatalf("a transaction was written for a refused installation: %v", err)
	}

	// A leftover transaction from an earlier run is refused instead of reused.
	if err := os.WriteFile(env.txFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write stale transaction: %v", err)
	}
	err = prepareApplicationInstall(env.target, env.txFile, true)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("prepare with a stale transaction: got %v, want a refusal", err)
	}
}
