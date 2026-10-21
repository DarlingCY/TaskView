//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func installedApplicationPath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Programs", "TaskView", "TaskView.exe"), nil
}

func launchUpdateInstaller(ctx context.Context, filename string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(filename, "/SP-", "/SILENT", "/SUPPRESSMSGBOXES", "/NORESTART")
	setDetachedProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start verified installer: %w", err)
	}
	// The installer outlives the tray it is about to stop. A separate reaper avoids
	// treating that expected shutdown as cancellation of the installation.
	go func() { _ = cmd.Wait() }()
	return nil
}
