//go:build windows

// Windows notification area icon.
//
// The icon is drawn by the shell through systray, which needs no CGO and no
// extra runtime: it is a plain Win32 tray icon.
package main

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

// The answers a message box can return.
const (
	messageBoxYes = 6
	messageBoxNo  = 7
)

// trayStartupTimeout bounds the wait for the icon to come up, and
// trayShutdownTimeout the wait for it to go away. A tray that failed to start
// must never keep the application alive.
const (
	trayStartupTimeout  = 3 * time.Second
	trayShutdownTimeout = 3 * time.Second
)

// runPlatformTray shows the TaskView icon in the notification area and keeps
// it there until the context is done or the user exits.
//
// It blocks for the lifetime of the icon and returns nil once the icon is
// gone.
func runPlatformTray(ctx context.Context, opts trayOptions) error {
	exited := make(chan struct{})
	ready := make(chan struct{})

	go func() {
		defer close(exited)
		// systray creates a hidden window and pumps its messages on the
		// calling thread. A goroutine may move between OS threads, which
		// would separate the window from its message pump, so the thread is
		// pinned for the lifetime of the tray.
		runtime.LockOSThread()
		systray.Run(func() {
			close(ready)
			startTrayMenu(opts)
		}, func() {})
	}()

	select {
	case <-ctx.Done():
	case <-exited:
		return nil
	}

	// The application is shutting down: remove the icon and let the message
	// loop end. The waits are bounded, because a tray that never came up must
	// not keep the application alive.
	select {
	case <-ready:
	case <-exited:
		return nil
	case <-time.After(trayStartupTimeout):
	}
	systray.Quit()

	select {
	case <-exited:
	case <-time.After(trayShutdownTimeout):
	}
	return nil
}

// startTrayMenu builds the notification area menu.
//
// It runs on systray's own goroutine, never on the message pump, so a slow
// action cannot freeze the icon.
func startTrayMenu(opts trayOptions) {
	systray.SetIcon(trayIcon)
	systray.SetTooltip("TaskView")

	// The version is information, not an action: the entry stays greyed out.
	version := systray.AddMenuItem(fmt.Sprintf("TaskView %s", appVersion()), "TaskView 版本")
	version.Disable()

	systray.AddSeparator()

	openItem := systray.AddMenuItem("打开看板", "在浏览器中打开 TaskView 看板")
	updateItem := systray.AddMenuItem(trayUpdateTitle, "检查是否有新版本")
	autostartItem := systray.AddMenuItemCheckbox("开机自启", "登录时自动启动 TaskView", false)

	systray.AddSeparator()

	exitItem := systray.AddMenuItem("退出", "退出 TaskView")

	ctl := newTrayController(opts, systrayMenuItem{autostartItem}, systrayMenuItem{updateItem})
	ctl.quit = systray.Quit
	ctl.run(systrayMenuItem{openItem}, systrayMenuItem{exitItem})

	// The menu is up. The autostart state is read in the background, so the
	// icon is usable right away.
	if opts.Ready != nil {
		opts.Ready()
	}
}

// systrayMenuItem adapts a systray menu entry to the controller's interfaces.
type systrayMenuItem struct {
	*systray.MenuItem
}

// Click returns the channel systray notifies on.
func (s systrayMenuItem) Click() <-chan struct{} { return s.ClickedCh }

// SetChecked shows or clears the check mark.
func (s systrayMenuItem) SetChecked(checked bool) {
	if checked {
		s.MenuItem.Check()
		return
	}
	s.MenuItem.Uncheck()
}

// SetEnabled greys the entry out or wakes it up.
func (s systrayMenuItem) SetEnabled(enabled bool) {
	if enabled {
		s.MenuItem.Enable()
		return
	}
	s.MenuItem.Disable()
}

// platformNotify shows the text in a native message box.
//
// It waits for the user to dismiss the box, so it is only called from a
// background goroutine.
func platformNotify(title, message string) {
	showMessageBox(title, message, windows.MB_OK|windows.MB_ICONINFORMATION)
}

// platformConfirm asks a yes/no question.
//
// No is the default answer, and a dialog that failed to open counts as No as
// well: an error must never be mistaken for consent.
func platformConfirm(title, message string) bool {
	flags := uint32(windows.MB_YESNO | windows.MB_ICONQUESTION | windows.MB_DEFBUTTON2)
	return showMessageBox(title, message, flags) == messageBoxYes
}

// showMessageBox shows a modal message box and returns the chosen button.
//
// A dialog that failed to open returns 0, which is neither Yes nor No.
func showMessageBox(title, message string, flags uint32) int {
	text, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return 0
	}
	caption, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return 0
	}
	result, _ := windows.MessageBox(0, text, caption, flags)
	return int(result)
}
