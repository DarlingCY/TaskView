//go:build !windows

// Notification area icon outside Windows.
//
// Only Windows owns a notification area icon. The other platforms keep the
// headless behaviour, so the application still runs everywhere it did before
// and nothing new shows up on the desktop.
package main

import (
	"context"
	"fmt"
	"os"
)

// runPlatformTray waits for the context and returns.
//
// Without a notification area there is nothing to show and nothing to clean
// up, so the call only follows the application.
func runPlatformTray(ctx context.Context, opts trayOptions) error {
	// There is no icon to wait for, so the tray is ready right away.
	if opts.Ready != nil {
		opts.Ready()
	}
	<-ctx.Done()
	return nil
}

// platformNotify writes the notification to stderr, where a headless run can
// show it.
func platformNotify(title, message string) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", title, message)
}

// platformConfirm has no dialog to ask with, so it answers No. Nothing that
// needs a decision may happen by accident.
func platformConfirm(title, message string) bool {
	return false
}
