//go:build !windows

// Install support for every target other than Windows.
//
// Install and Uninstall copy the binary to <data-dir>/bin/taskview with owner
// only permissions. Autostart is refused before anything is written, because
// there is no dependency-free cross-platform way to register a per-user login
// item.
package main

import (
	"fmt"
	"os"
)

func platformAutostartSupported() bool { return false }

func platformBinaryName() (string, error) { return "taskview", nil }

// platformPostCopy restricts the freshly written executable to the owner.
func platformPostCopy(path string) error {
	return os.Chmod(path, 0o700)
}

// platformProtectFile restricts a manifest to the owner.
func platformProtectFile(path string) error {
	return os.Chmod(path, 0o600)
}

// platformRegisterAutostart is never reached: Install rejects Autostart up front
// with ErrAutostartUnsupported.
func platformRegisterAutostart(managed, dataDir string) error {
	return ErrAutostartUnsupported
}

// platformRemoveAutostart is a no-op because nothing can have registered a task.
func platformRemoveAutostart(dataDir string) error { return nil }

// platformRunTaskScheduler is unreachable: Install rejects --autostart before
// registration is attempted.
func platformRunTaskScheduler(mode, taskName, xmlDoc string) error {
	return ErrAutostartUnsupported
}

// platformValidateTaskDefinition is a no-op: there is no Task Scheduler here.
func platformValidateTaskDefinition(xmlDoc string) error { return nil }

// platformReplaceExecutable moves src to dst atomically.
func platformReplaceExecutable(src, dst string) error {
	return os.Rename(src, dst)
}

// platformRemoveInstalled deletes the managed executable.
//
// POSIX unlinks a running executable successfully, so this never needs the
// deferred-helper fallback Windows requires and there is no self-delete path
// here at all.
func platformRemoveInstalled(managed string) error {
	err := os.Remove(managed)
	switch {
	case err == nil, os.IsNotExist(err):
		return nil
	default:
		return fmt.Errorf("remove %s: %w", managed, err)
	}
}
