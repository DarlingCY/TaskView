//go:build !windows

// Non-Windows platform stubs.
//
// The application itself is the Windows tray app; these stubs exist so the
// repository's root package still compiles on Linux and macOS (the board HTTP
// layer and the CLI are the cross-platform parts). Every operation that is
// Windows-specific reports that it is unavailable instead of pretending to
// work. Nothing in this file is compiled on Windows, so the Windows behaviour is
// untouched.

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// errPlatformUnsupported is returned by the Windows-only operations.
var errPlatformUnsupported = errors.New("this operation is only available in the Windows build")

// ---------------------------------------------------------------------------
// Autostart
// ---------------------------------------------------------------------------

func platformAutostartRegistration(dataDir string) (autostartRegistration, error) {
	return autostartRegistration{}, errPlatformUnsupported
}

func platformAutostartEnabled(exe, dataDir string) (bool, error) {
	return false, errPlatformUnsupported
}

func platformAutostartApply(exe, dataDir string, enabled bool) error {
	return errPlatformUnsupported
}

func platformRestoreAutostart(dataDir, definition string) error {
	return errPlatformUnsupported
}

// ---------------------------------------------------------------------------
// Notification area and dialogs
// ---------------------------------------------------------------------------

func runPlatformTray(ctx context.Context, opts trayOptions) error {
	return errPlatformUnsupported
}

func platformNotify(title, message string) {
	// There is no notification area outside Windows; the caller has already
	// written the failure to its log.
	_, _ = os.Stderr.WriteString(title + ": " + message + "\n")
}

func platformConfirm(title, message string) bool {
	// An unavailable dialog is never consent.
	return false
}

// ---------------------------------------------------------------------------
// Update installer
// ---------------------------------------------------------------------------

func launchUpdateInstaller(ctx context.Context, filename string) error {
	return errPlatformUnsupported
}

func installedApplicationPath() (string, error) {
	return "", errPlatformUnsupported
}

// ---------------------------------------------------------------------------
// Process lock and detachment
// ---------------------------------------------------------------------------

type osFileLock struct {
	file *os.File
}

func acquireOSLock(filename string) (*osFileLock, error) {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &osFileLock{file: f}, nil
}

func releaseOSLock(lock *osFileLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	return lock.file.Close()
}

func setDetachedProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// ---------------------------------------------------------------------------
// Install lifecycle
// ---------------------------------------------------------------------------

func platformRunTaskScheduler(mode, taskName, xmlDoc string) error {
	return errPlatformUnsupported
}

func platformValidateTaskDefinition(xmlDoc string) error {
	return errPlatformUnsupported
}

func platformBinaryName() (string, error) { return "taskview", nil }

func platformAutostartDefinition(managed, dataDir string, enabled bool) (string, error) {
	return "", errPlatformUnsupported
}

func platformRegisterAutostart(managed, dataDir string) error {
	return errPlatformUnsupported
}

func platformRemoveAutostart(dataDir string) error {
	return errPlatformUnsupported
}

func platformRemoveInstalled(managed string) error {
	if err := os.Remove(managed); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func platformPostCopy(path string) error { return nil }

func platformProtectFile(path string) error { return nil }

func platformReplaceExecutable(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return nil
}
