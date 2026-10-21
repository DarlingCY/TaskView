package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const legacyMigrationExitCode = 20

type applicationExitError struct {
	Code int
	Err  error
}

func (e *applicationExitError) Error() string { return e.Err.Error() }
func (e *applicationExitError) Unwrap() error { return e.Err }

// The installer and a short-lived copy of the application communicate only in
// its private temporary directory. The worker holds the application locks while
// Inno replaces files; it never opens the task database.
type installTransaction struct {
	Target         string `json:"target"`
	DataDir        string `json:"data_dir"`
	Version        string `json:"version"`
	Directory      string `json:"directory"`
	Phase          string `json:"phase"`
	Error          string `json:"error,omitempty"`
	AllowLegacy    bool   `json:"allow_legacy"`
	HadApplication bool   `json:"had_application"`
	WasRunning     bool   `json:"was_running"`
	OldApplication string `json:"old_application,omitempty"`
	OldVersion     string `json:"old_version,omitempty"`
	StartupXML     string `json:"startup_xml,omitempty"`
	Autostart      bool   `json:"autostart"`
	HadManifest    bool   `json:"had_manifest"`
}

type installDecision struct {
	Action    string `json:"action"`
	Autostart string `json:"autostart"`
	Start     string `json:"start"`
}

var installationStart = startInstalledApplication
var installationHealth = waitForApplicationVersion
var installationRegistration = func(dir string) (autostartRegistration, error) { return autostartRegistrationReader(dir) }

func validateInstallTarget(target string) error {
	expected, err := installedApplicationPath()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) || !samePath(target, expected) {
		return errors.New("installer target is not the fixed per-user TaskView installation path")
	}
	return nil
}

func validateTransactionPath(name string) error {
	if !filepath.IsAbs(name) {
		return errors.New("transaction path must be absolute")
	}
	temp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(temp, filepath.Clean(name))
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return errors.New("installer transaction must be in a temporary directory")
	}
	if filepath.Base(name) != "taskview-transaction.json" {
		return errors.New("invalid installer transaction filename")
	}
	return nil
}

func readInstallTransaction(name string) (*installTransaction, error) {
	if err := validateTransactionPath(name); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if len(raw) > 1024*1024 {
		return nil, errors.New("installation transaction is too large")
	}
	var tx installTransaction
	if err = json.Unmarshal(raw, &tx); err != nil {
		return nil, err
	}
	if err = validateInstallTarget(tx.Target); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(tx.DataDir) || !filepath.IsAbs(tx.Directory) || !samePath(filepath.Dir(tx.Directory), filepath.Dir(name)) || !strings.HasPrefix(filepath.Base(tx.Directory), "taskview-install-") {
		return nil, errors.New("invalid installation transaction paths")
	}
	return &tx, nil
}

func runInstallMode(o applicationOptions) error {
	name := o.values["--transaction"]
	switch o.mode {
	case "--internal-install-prepare":
		return prepareApplicationInstall(o.values["--target"], name, o.values["--allow-legacy"] == "true")
	case "--internal-install-worker":
		return applicationInstallWorker(name)
	case "--internal-install-commit", "--internal-install-abort":
		if _, err := readInstallTransaction(name); err != nil {
			return err
		}
		decision := installDecision{Action: "abort"}
		if o.mode == "--internal-install-commit" {
			decision = installDecision{Action: "commit", Autostart: o.values["--autostart"], Start: o.values["--start"]}
			if decision.Autostart != "enable" && decision.Autostart != "preserve" {
				return errors.New("invalid startup decision")
			}
			if decision.Start != "yes" && decision.Start != "no" {
				return errors.New("invalid launch decision")
			}
		}
		if err := writeJSONAtomic(name+".decision", decision); err != nil {
			return err
		}
		_, err := waitInstallPhase(name, 3*time.Minute, "complete", "aborted", "error")
		return err
	case "--internal-uninstall":
		return uninstallApplication(o.values["--target"])
	}
	return errors.New("unknown installation mode")
}

func prepareApplicationInstall(target, name string, allowLegacy bool) error {
	if err := validateInstallTarget(target); err != nil {
		return err
	}
	if err := validateTransactionPath(name); err != nil {
		return err
	}
	if _, err := os.Lstat(name); err == nil {
		return errors.New("installation transaction already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	dir, err := DataDir()
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	reg, err := installationRegistration(dir)
	if err != nil {
		return err
	}
	manifest, err := readInstallManifest(dir)
	if err != nil {
		return err
	}
	if err = validatePreviousInstallation(dir, target, manifest, reg, allowLegacy); err != nil {
		return err
	}
	transactionDir, err := os.MkdirTemp(filepath.Dir(name), "taskview-install-")
	if err != nil {
		return err
	}
	tx := installTransaction{Target: target, DataDir: dir, Version: appVersion(), Directory: transactionDir, Phase: "starting", AllowLegacy: allowLegacy}
	if err = writeJSONAtomic(name, tx); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "--internal-install-worker", "--transaction", name, "--data-dir", dir)
	setDetachedProcAttr(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	// The worker may have to wait for the install lock, stop a running
	// instance and read the Task Scheduler before it is ready.
	_, err = waitInstallPhase(name, 60*time.Second, "ready", "error")
	return err
}

func waitInstallPhase(name string, timeout time.Duration, phases ...string) (*installTransaction, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tx, err := readInstallTransaction(name)
		if err != nil {
			return nil, err
		}
		for _, phase := range phases {
			if tx.Phase == phase {
				if tx.Error != "" {
					return tx, errors.New(tx.Error)
				}
				return tx, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("installation coordinator timed out; the previous version has been left for recovery")
}

func validatePreviousInstallation(dir, target string, manifest installManifest, reg autostartRegistration, allowLegacy bool) error {
	legacy, err := installTargetPath(dir)
	if err != nil {
		return err
	}
	if manifest.Path != "" && !samePath(manifest.Path, legacy) && !samePath(manifest.Path, target) {
		return errors.New("unrecognized TaskView installation manifest")
	}
	if reg.Exists && (!reg.MatchesDataDir || (!samePath(reg.Exe, legacy) && !samePath(reg.Exe, target)) || reg.XML == "") {
		return errors.New("the TaskView logon task has an unrecognized action or no recoverable definition")
	}
	if (sameNonemptyPath(manifest.Path, legacy) || reg.Exists && samePath(reg.Exe, legacy)) && !allowLegacy {
		return &applicationExitError{legacyMigrationExitCode, errors.New("a legacy TaskView installation was found; confirm migration before continuing")}
	}
	if sameNonemptyPath(manifest.Path, target) && manifest.Version != "" {
		previous, e := parseStableVersion(manifest.Version)
		current, c := parseStableVersion(appVersion())
		if e != nil || c != nil {
			return errors.New("cannot replace an installed release with an unversioned build")
		}
		if current.compare(previous) < 0 {
			return errors.New("installation downgrade is not supported")
		}
	}
	return nil
}

func sameNonemptyPath(a, b string) bool { return a != "" && b != "" && samePath(a, b) }

func applicationInstallWorker(name string) (result error) {
	tx, err := readInstallTransaction(name)
	if err != nil {
		return err
	}
	if err = os.Setenv("TASKVIEW_HOME", tx.DataDir); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			tx.Phase = "error"
			tx.Error = result.Error()
			_ = writeJSONAtomic(name, tx)
		}
	}()
	unlock, err := acquireInstallLock(filepath.Join(tx.DataDir, installLockName), 15*time.Second)
	if err != nil {
		return err
	}
	installHeld := true
	releaseInstall := func() {
		if installHeld {
			unlock()
			installHeld = false
		}
	}
	defer releaseInstall()
	reg, err := installationRegistration(tx.DataDir)
	if err != nil {
		return err
	}
	manifest, err := readInstallManifest(tx.DataDir)
	if err != nil {
		return err
	}
	if err = validatePreviousInstallation(tx.DataDir, tx.Target, manifest, reg, tx.AllowLegacy); err != nil {
		return err
	}
	tx.StartupXML, tx.Autostart = reg.XML, reg.Exists && reg.Enabled
	tx.OldApplication = tx.Target
	legacy, err := installTargetPath(tx.DataDir)
	if err != nil {
		return err
	}
	if tx.AllowLegacy && (sameNonemptyPath(manifest.Path, legacy) || reg.Exists && samePath(reg.Exe, legacy)) {
		tx.OldApplication = legacy
	}
	if current, err := ManagedServerStatus(); err != nil {
		return err
	} else if current != nil {
		tx.WasRunning, tx.OldVersion = true, current.Version
		if current.Executable != "" {
			if !samePath(current.Executable, tx.Target) && !(tx.AllowLegacy && samePath(current.Executable, legacy)) {
				return errors.New("the running TaskView is not the installed application; close it before installing")
			}
			tx.OldApplication = current.Executable
		} else if !tx.AllowLegacy {
			return errors.New("close the older TaskView instance before installing")
		}
	}
	if info, err := os.Lstat(tx.Target); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("installed executable is not a regular file")
		}
		tx.HadApplication = true
		if err = copyInstallBackup(tx.Target, filepath.Join(tx.Directory, "TaskView.exe.bak")); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if raw, err := os.ReadFile(installManifestPath(tx.DataDir)); err == nil {
		tx.HadManifest = true
		if err = os.WriteFile(filepath.Join(tx.Directory, "install.json.bak"), raw, 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	serverUnlock, err := stopManagedServerForInstall()
	if err != nil {
		return err
	}
	locksHeld := true
	releaseServer := func() {
		if locksHeld {
			serverUnlock()
			locksHeld = false
		}
	}
	defer releaseServer()
	settled := false
	defer func() {
		if result != nil && !settled {
			releaseServer()
			if recoveryErr := rollbackApplication(tx); recoveryErr != nil {
				result = fmt.Errorf("%v; recovery failed: %w", result, recoveryErr)
			}
		}
	}()
	tx.Phase = "ready"
	if err = writeJSONAtomic(name, tx); err != nil {
		return err
	}
	// The decision normally arrives within seconds. The bound is short so a
	// crashed installer cannot keep the install lock and a stopped server for
	// long.
	deadline := time.Now().Add(2 * time.Minute)
	var decision installDecision
	decided := false
	for time.Now().Before(deadline) {
		raw, e := os.ReadFile(name + ".decision")
		if e == nil {
			if len(raw) > 4096 {
				return errors.New("invalid installer decision")
			}
			if e = json.Unmarshal(raw, &decision); e != nil {
				return e
			}
			decided = true
			break
		}
		if !os.IsNotExist(e) {
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !decided {
		return errors.New("the installer did not confirm the installation; the previous version is left in place")
	}
	if decision.Action != "commit" {
		releaseServer()
		if err = rollbackApplication(tx); err != nil {
			return err
		}
		settled = true
		tx.Phase = "aborted"
		return writeJSONAtomic(name, tx)
	}
	if decision.Autostart != "enable" && decision.Autostart != "preserve" {
		return errors.New("invalid installation startup action")
	}
	if decision.Start != "yes" && decision.Start != "no" {
		return errors.New("invalid installation launch action")
	}
	// "preserve" keeps the registration as it is, including one the user
	// switched off in the Task Scheduler. Only an existing enabled registration
	// is carried over as enabled; a disabled one is re-registered against the
	// new executable but stays disabled.
	wantStartup := decision.Autostart == "enable" || (decision.Autostart == "preserve" && reg.Exists && reg.Enabled)
	keepDisabled := decision.Autostart == "preserve" && reg.Exists && !reg.Enabled
	switch {
	case wantStartup:
		err = platformRegisterAutostart(tx.Target, tx.DataDir)
	case keepDisabled && samePath(reg.Exe, tx.Target):
		err = restoreAutostart(tx.DataDir, tx.StartupXML)
	case keepDisabled:
		var xml string
		xml, err = platformAutostartDefinition(tx.Target, tx.DataDir, false)
		if err == nil {
			err = restoreAutostart(tx.DataDir, xml)
		}
	default:
		if reg.Exists || tx.StartupXML != "" {
			err = platformRemoveAutostart(tx.DataDir)
		}
	}
	if err == nil {
		err = writeInstallManifest(tx.DataDir, &installManifest{Path: tx.Target, Autostart: wantStartup, InstalledAt: time.Now().UTC().Format(time.RFC3339), Version: tx.Version})
	}
	releaseServer()
	// The files are in place and the manifest is written. The installed
	// application is about to start, and its tray reads the autostart state
	// through the install lock: holding that lock while waiting for the new
	// instance to answer would make the first menu refresh wait for us.
	releaseInstall()
	if err == nil && (tx.WasRunning || decision.Start == "yes") {
		err = installationStart(tx.Target, tx.DataDir)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = installationHealth(ctx, tx.Version, tx.Target)
			cancel()
		}
	}
	if err != nil {
		// The deferred recovery below restores the previous executable,
		// manifest and logon task, exactly once.
		return fmt.Errorf("upgrade failed: %w", err)
	}
	// The upgrade succeeded: nothing may roll it back any more, not even a
	// failure to record the final phase.
	settled = true
	tx.Phase = "complete"
	return writeJSONAtomic(name, tx)
}

// legacyInstallationPending reports whether an installation from the previous
// major version is still present.
//
// The silent upgrade must never migrate it: it stops at the prepare step with
// exit code 20 and the user would otherwise see nothing at all. The update
// check asks this first and explains what to do instead.
func legacyInstallationPending() (bool, error) {
	dir, err := DataDir()
	if err != nil {
		return false, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	reg, err := installationRegistration(dir)
	if err != nil {
		return false, err
	}
	manifest, err := readInstallManifest(dir)
	if err != nil {
		return false, err
	}
	legacy, err := installTargetPath(dir)
	if err != nil {
		return false, err
	}
	if sameNonemptyPath(manifest.Path, legacy) || reg.Exists && samePath(reg.Exe, legacy) {
		return true, nil
	}
	return reg.Exists && !reg.MatchesDataDir, nil
}

func copyInstallBackup(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o600)
}

func rollbackApplication(tx *installTransaction) error {
	stopRelease, err := stopManagedServerForInstall()
	if err != nil {
		return err
	}
	if tx.HadApplication {
		err = replaceExecutable(filepath.Join(tx.Directory, "TaskView.exe.bak"), tx.Target)
	} else {
		// No previous file is restored on a cancelled first install; Inno owns cleanup.
		if _, statErr := os.Stat(tx.Target); statErr == nil {
			err = os.Remove(tx.Target)
		}
	}
	if err == nil {
		if tx.HadManifest {
			var raw []byte
			raw, err = os.ReadFile(filepath.Join(tx.Directory, "install.json.bak"))
			if err == nil {
				err = os.WriteFile(installManifestPath(tx.DataDir), raw, 0o600)
			}
		} else {
			if e := os.Remove(installManifestPath(tx.DataDir)); e != nil && !os.IsNotExist(e) {
				err = e
			}
		}
	}
	if err == nil {
		if tx.StartupXML != "" {
			err = restoreAutostart(tx.DataDir, tx.StartupXML)
		} else {
			err = platformRemoveAutostart(tx.DataDir)
		}
	}
	stopRelease()
	if err != nil {
		return err
	}
	if tx.WasRunning && tx.OldApplication != "" {
		if err = installationStart(tx.OldApplication, tx.DataDir); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return installationHealth(ctx, tx.OldVersion, tx.OldApplication)
	}
	return nil
}

func startInstalledApplication(exe, dir string) error {
	args := []string{"--internal-run", "--data-dir", dir}
	legacy, err := installTargetPath(dir)
	if err != nil {
		return err
	}
	if samePath(exe, legacy) {
		// The portable CLI of the previous major version starts its managed
		// server with the "server run" command.
		args = []string{"server", "run", "--port", "3333", "--data-dir", dir}
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "TASKVIEW_HOME="+dir)
	cmd.Dir = filepath.Dir(exe)
	if logFile, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stdout, cmd.Stderr = logFile, logFile
		defer logFile.Close()
	}
	setDetachedProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func uninstallApplication(target string) error {
	if err := validateInstallTarget(target); err != nil {
		return err
	}
	dir, err := DataDir()
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	unlock, err := acquireInstallLock(filepath.Join(dir, installLockName), 15*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	reg, err := installationRegistration(dir)
	if err != nil {
		return err
	}
	if reg.Exists && (!samePath(reg.Exe, target) || !reg.MatchesDataDir) {
		return errors.New("the startup task belongs to another installation; refusing removal")
	}
	stopRelease, err := stopManagedServerForInstall()
	if err != nil {
		return err
	}
	defer stopRelease()
	if reg.Exists {
		if err = platformRemoveAutostart(dir); err != nil {
			return err
		}
	}
	manifest, err := readInstallManifest(dir)
	if err != nil {
		return err
	}
	if samePath(manifest.Path, target) {
		if err = os.Remove(installManifestPath(dir)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
