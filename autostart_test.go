package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestQueryAutostartTakesTheInstallLockAndDelegates proves that the query
// serialises against a running install and asks the platform for the state.
func TestQueryAutostartTakesTheInstallLockAndDelegates(t *testing.T) {
	dir := t.TempDir()

	var locked, released bool
	origLock := acquireInstallLock
	acquireInstallLock = func(filename string, timeout time.Duration) (func(), error) {
		locked = true
		if want := filepath.Join(dir, installLockName); filename != want {
			t.Errorf("lock file = %q, want %q", filename, want)
		}
		return func() { released = true }, nil
	}
	t.Cleanup(func() { acquireInstallLock = origLock })

	origReader := autostartStateReader
	autostartStateReader = func(exe, dataDir string) (bool, error) { return true, nil }
	t.Cleanup(func() { autostartStateReader = origReader })

	on, err := queryAutostart(`C:\app\taskview.exe`, dir)
	if err != nil {
		t.Fatalf("queryAutostart: %v", err)
	}
	if !on {
		t.Error("the platform reported an enabled autostart")
	}
	if !locked {
		t.Error("queryAutostart must take the install lock")
	}
	if !released {
		t.Error("the install lock must be released again")
	}
}

// TestQueryAutostartReportsLockFailure proves that a busy lock is reported
// instead of being ignored.
func TestQueryAutostartReportsLockFailure(t *testing.T) {
	origLock := acquireInstallLock
	acquireInstallLock = func(string, time.Duration) (func(), error) {
		return nil, errors.New("lock is held")
	}
	t.Cleanup(func() { acquireInstallLock = origLock })

	if _, err := queryAutostart(`C:\app\taskview.exe`, t.TempDir()); err == nil {
		t.Error("queryAutostart must report a lock failure")
	}
}

// TestSetAutostartDelegatesUnderTheInstallLock proves that switching the
// autostart goes through the platform while the install lock is held.
func TestSetAutostartDelegatesUnderTheInstallLock(t *testing.T) {
	dir := t.TempDir()

	var locked bool
	origLock := acquireInstallLock
	acquireInstallLock = func(string, time.Duration) (func(), error) {
		locked = true
		return func() {}, nil
	}
	t.Cleanup(func() { acquireInstallLock = origLock })

	var gotEnabled bool
	var gotExe string
	origWriter := autostartWriter
	autostartWriter = func(exe, dataDir string, enabled bool) error {
		gotExe, gotEnabled = exe, enabled
		return nil
	}
	t.Cleanup(func() { autostartWriter = origWriter })

	if err := setAutostart(`C:\app\taskview.exe`, dir, true); err != nil {
		t.Fatalf("setAutostart: %v", err)
	}
	if !locked {
		t.Error("setAutostart must take the install lock")
	}
	if !gotEnabled {
		t.Error("the autostart should have been enabled")
	}
	if gotExe != `C:\app\taskview.exe` {
		t.Errorf("exe = %q, want the executable that was asked for", gotExe)
	}
}

// TestSetAutostartCanDisable proves that switching the autostart off is a
// plain change of state and not an uninstall.
func TestSetAutostartCanDisable(t *testing.T) {
	origLock := acquireInstallLock
	acquireInstallLock = func(string, time.Duration) (func(), error) { return func() {}, nil }
	t.Cleanup(func() { acquireInstallLock = origLock })

	var gotEnabled = true
	origWriter := autostartWriter
	autostartWriter = func(exe, dataDir string, enabled bool) error {
		gotEnabled = enabled
		return nil
	}
	t.Cleanup(func() { autostartWriter = origWriter })

	if err := setAutostart(`C:\app\taskview.exe`, t.TempDir(), false); err != nil {
		t.Fatalf("setAutostart: %v", err)
	}
	if gotEnabled {
		t.Error("the autostart should have been disabled")
	}
}

// TestReadAutostartRegistrationReportsThePlatformFacts proves that the
// registration is reported as the platform sees it.
func TestReadAutostartRegistrationReportsThePlatformFacts(t *testing.T) {
	orig := autostartRegistrationReader
	autostartRegistrationReader = func(dataDir string) (autostartRegistration, error) {
		return autostartRegistration{
			Exe:     `C:\app\taskview.exe`,
			Args:    []string{"--background", "--data-dir", dataDir},
			Actions: 1,
			Enabled: true,
			Exists:  true,
		}, nil
	}
	t.Cleanup(func() { autostartRegistrationReader = orig })

	exe, enabled, exists, err := readAutostartRegistration(t.TempDir())
	if err != nil {
		t.Fatalf("readAutostartRegistration: %v", err)
	}
	if exe != `C:\app\taskview.exe` {
		t.Errorf("exe = %q, want the registered executable", exe)
	}
	if !enabled {
		t.Error("the registration should be reported as enabled")
	}
	if !exists {
		t.Error("the registration should be reported as existing")
	}
}

// TestReadAutostartRegistrationDoesNotTakeTheInstallLock proves that the
// read can be used from an install that already holds the lock. The lock is
// not reentrant, so taking it twice would deadlock.
func TestReadAutostartRegistrationDoesNotTakeTheInstallLock(t *testing.T) {
	origLock := acquireInstallLock
	acquireInstallLock = func(string, time.Duration) (func(), error) {
		t.Error("the registration read must not take the install lock")
		return func() {}, nil
	}
	t.Cleanup(func() { acquireInstallLock = origLock })

	orig := autostartRegistrationReader
	autostartRegistrationReader = func(dataDir string) (autostartRegistration, error) {
		return autostartRegistration{Exists: true, Enabled: true}, nil
	}
	t.Cleanup(func() { autostartRegistrationReader = orig })

	if _, _, _, err := readAutostartRegistration(t.TempDir()); err != nil {
		t.Fatalf("readAutostartRegistration: %v", err)
	}
	if _, err := snapshotAutostart(t.TempDir()); err != nil {
		t.Fatalf("snapshotAutostart: %v", err)
	}
}

// TestSnapshotAutostartReturnsNothingWhenAbsent proves that a missing
// registration is reported as "nothing to restore" instead of an error.
func TestSnapshotAutostartReturnsNothingWhenAbsent(t *testing.T) {
	orig := autostartRegistrationReader
	autostartRegistrationReader = func(dataDir string) (autostartRegistration, error) {
		return autostartRegistration{}, nil
	}
	t.Cleanup(func() { autostartRegistrationReader = orig })

	xml, err := snapshotAutostart(t.TempDir())
	if err != nil {
		t.Fatalf("snapshotAutostart: %v", err)
	}
	if xml != "" {
		t.Errorf("snapshot = %q, want an empty snapshot", xml)
	}
}

// TestSnapshotAutostartKeepsTheRegistration proves that a disabled
// registration can be restored exactly as it was.
func TestSnapshotAutostartKeepsTheRegistration(t *testing.T) {
	const doc = `<?xml version="1.0" encoding="UTF-16"?><Task><Settings><Enabled>false</Enabled></Settings></Task>`

	orig := autostartRegistrationReader
	autostartRegistrationReader = func(dataDir string) (autostartRegistration, error) {
		return autostartRegistration{Exists: true, Enabled: false, XML: doc}, nil
	}
	t.Cleanup(func() { autostartRegistrationReader = orig })

	xml, err := snapshotAutostart(t.TempDir())
	if err != nil {
		t.Fatalf("snapshotAutostart: %v", err)
	}
	if xml != doc {
		t.Errorf("snapshot = %q, want the registered definition", xml)
	}
}

// TestRestoreAutostartDoesNotTakeTheInstallLock proves that the restore can
// be used from an install that already holds the lock, and that it reaches
// the platform.
func TestRestoreAutostartDoesNotTakeTheInstallLock(t *testing.T) {
	origLock := acquireInstallLock
	acquireInstallLock = func(string, time.Duration) (func(), error) {
		t.Error("the restore must not take the install lock")
		return func() {}, nil
	}
	t.Cleanup(func() { acquireInstallLock = origLock })

	var gotXML string
	origRestorer := autostartRestorer
	autostartRestorer = func(dataDir, xml string) error {
		gotXML = xml
		return nil
	}
	t.Cleanup(func() { autostartRestorer = origRestorer })

	if err := restoreAutostart(t.TempDir(), "<Task/>"); err != nil {
		t.Fatalf("restoreAutostart: %v", err)
	}
	if gotXML != "<Task/>" {
		t.Errorf("restored %q, want the captured definition", gotXML)
	}
}

// TestAutostartRegistrationEnablesNeedsEveryCondition proves that a task
// which is switched off, missing or pointing somewhere else does not count as
// an enabled autostart.
func TestAutostartRegistrationEnablesNeedsEveryCondition(t *testing.T) {
	full := autostartRegistration{
		Exe:            `C:\app\taskview.exe`,
		Args:           []string{"--background", "--data-dir", `C:\data`},
		Actions:        1,
		Enabled:        true,
		Exists:         true,
		MatchesDataDir: true,
	}
	if !full.Enables(`C:\app\taskview.exe`) {
		t.Error("a matching registration must count as enabled")
	}
	if !full.Enables(`c:\app\taskview.exe`) {
		t.Error("Windows paths are case-insensitive")
	}
	if full.Enables(`C:\other\taskview.exe`) {
		t.Error("a task launching another executable must not count")
	}

	disabled := full
	disabled.Enabled = false
	if disabled.Enables(`C:\app\taskview.exe`) {
		t.Error("a disabled task must not count as enabled")
	}

	absent := full
	absent.Exists = false
	if absent.Enables(`C:\app\taskview.exe`) {
		t.Error("a missing task must not count as enabled")
	}

	otherDir := full
	otherDir.MatchesDataDir = false
	if otherDir.Enables(`C:\app\taskview.exe`) {
		t.Error("a task pointing at another data directory must not count")
	}

	twoActions := full
	twoActions.Actions = 2
	if twoActions.Enables(`C:\app\taskview.exe`) {
		t.Error("a task with more than one action must not count")
	}
}

// TestMatchesDataDirComparesArguments proves that the data directory is
// compared as an argument and not as a piece of text, and that only TaskView's
// own launch forms count.
func TestMatchesDataDirComparesArguments(t *testing.T) {
	if !matchesDataDir([]string{"--background", "--data-dir", `C:\data`}, `C:\data`) {
		t.Error("the background launch form must match")
	}
	if !matchesDataDir([]string{"server", "start", "--port", "3333", "--data-dir", `C:\data`}, `C:\data`) {
		t.Error("the legacy launch form must match")
	}
	if !matchesDataDir([]string{"--background", "--data-dir=" + `C:\data`}, `C:\data`) {
		t.Error("the switch=value form must match")
	}

	// A data directory that is only the prefix of another one must not match.
	if matchesDataDir([]string{"--background", "--data-dir", `C:\data-other`}, `C:\data`) {
		t.Error("a data directory that is only a prefix must not match")
	}
	if matchesDataDir([]string{"--background", "--data-dir", `C:\data`}, `C:\data-other`) {
		t.Error("a data directory that is only a prefix must not match")
	}
	if matchesDataDir([]string{"--background"}, `C:\data`) {
		t.Error("a command line without a data directory must not match")
	}
	if matchesDataDir([]string{"--notebook", "--data-dir", `C:\data`}, `C:\data`) {
		t.Error("a command line TaskView did not install must not match")
	}
	if matchesDataDir([]string{"server", "stop", "--data-dir", `C:\data`}, `C:\data`) {
		t.Error("an unknown server command must not match")
	}
	if matchesDataDir(nil, `C:\data`) {
		t.Error("an empty command line must not match")
	}
}
