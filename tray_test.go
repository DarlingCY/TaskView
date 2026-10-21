package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMenuItem is a menu entry that records what the controller did with it.
//
// It stands in for a real notification area entry, so the tests never create
// an icon.
type fakeMenuItem struct {
	clicks chan struct{}

	mu      sync.Mutex
	checked bool
	enabled bool
	title   string
}

func newFakeMenuItem() *fakeMenuItem {
	return &fakeMenuItem{clicks: make(chan struct{}, 8), enabled: true}
}

func (f *fakeMenuItem) Click() <-chan struct{} { return f.clicks }

func (f *fakeMenuItem) SetChecked(checked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = checked
}

func (f *fakeMenuItem) SetEnabled(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = enabled
}

func (f *fakeMenuItem) SetTitle(title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.title = title
}

func (f *fakeMenuItem) Checked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checked
}

func (f *fakeMenuItem) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

func (f *fakeMenuItem) Title() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.title
}

// Activate simulates a click on the entry.
func (f *fakeMenuItem) Activate() { f.clicks <- struct{}{} }

// notificationRecorder collects the notifications the controller sends.
type notificationRecorder struct {
	mu   sync.Mutex
	list []string
}

func (n *notificationRecorder) notify(title, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, message)
}

func (n *notificationRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.list)
}

func (n *notificationRecorder) text() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.list, " | ")
}

// waitFor waits until cond holds, so an asynchronous action can be observed
// without a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testController builds a controller whose platform calls are fakes.
func testController(rec *notificationRecorder) (*trayController, *fakeMenuItem, *fakeMenuItem) {
	autostartItem := newFakeMenuItem()
	updateItem := newFakeMenuItem()
	return &trayController{
		opts:          trayOptions{URL: "http://127.0.0.1:3333", DataDir: `C:\data`},
		exePath:       func() (string, error) { return `C:\app\taskview.exe`, nil },
		query:         func(string, string) (bool, error) { return false, nil },
		set:           func(string, string, bool) error { return nil },
		notify:        rec.notify,
		open:          func(string) bool { return true },
		quit:          func() {},
		autostartItem: autostartItem,
		updateItem:    updateItem,
	}, autostartItem, updateItem
}

// TestTrayControllerEnablesAutostartAndShowsIt proves that the toggle asks
// the platform to switch the autostart on and puts the new state into the
// menu.
func TestTrayControllerEnablesAutostartAndShowsIt(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, item, _ := testController(rec)

	var calls atomic.Int32
	var wanted atomic.Bool
	ctl.set = func(exe, dataDir string, enabled bool) error {
		calls.Add(1)
		wanted.Store(enabled)
		return nil
	}

	ctl.toggleAutostart()
	waitFor(t, "the scheduler call", func() bool { return calls.Load() == 1 })

	if !wanted.Load() {
		t.Error("the autostart should have been enabled")
	}
	waitFor(t, "the menu to show the new state", item.Checked)
	if rec.count() != 0 {
		t.Errorf("unexpected notification: %s", rec.text())
	}
}

// TestTrayControllerKeepsRealAutostartStateAfterFailure proves that a failed
// switch leaves the menu showing what is really registered, not what was
// asked for.
func TestTrayControllerKeepsRealAutostartStateAfterFailure(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, item, _ := testController(rec)

	// The task is registered and enabled; the switch off fails.
	ctl.query = func(string, string) (bool, error) { return true, nil }
	ctl.set = func(string, string, bool) error { return errors.New("access denied") }
	ctl.setAutostartChecked(true)

	ctl.toggleAutostart()
	waitFor(t, "the failure report", func() bool { return rec.count() > 0 })

	if !strings.Contains(rec.text(), "access denied") {
		t.Errorf("the failure should be reported, got %q", rec.text())
	}
	waitFor(t, "the real state to be shown", item.Checked)
}

// TestTrayControllerIgnoresConcurrentAutostartToggle proves that a second
// toggle while the first one is still working does not start a second
// scheduler call.
func TestTrayControllerIgnoresConcurrentAutostartToggle(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, _, _ := testController(rec)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	ctl.set = func(string, string, bool) error {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}

	ctl.toggleAutostart()
	<-started
	ctl.toggleAutostart()

	if got := calls.Load(); got != 1 {
		t.Fatalf("the second toggle started another scheduler call: %d calls", got)
	}
	close(release)
}

// TestTrayControllerRunsUpdateCheckInBackgroundOnlyOnce proves that the
// check runs off the UI thread and that a second click while it is running is
// ignored.
func TestTrayControllerRunsUpdateCheckInBackgroundOnlyOnce(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, _, updateItem := testController(rec)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	ctl.opts.CheckUpdate = func(ctx context.Context) error {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}

	// The call must return while the check is still running: the menu thread
	// cannot be blocked by the network.
	ctl.checkForUpdate()
	<-started

	ctl.checkForUpdate()
	if got := calls.Load(); got != 1 {
		t.Fatalf("a second update check was started: %d calls", got)
	}
	if updateItem.Title() != trayUpdateRunningTitle {
		t.Errorf("menu title = %q, want %q", updateItem.Title(), trayUpdateRunningTitle)
	}

	close(release)
	waitFor(t, "the menu to be usable again", updateItem.Enabled)
	if updateItem.Title() != trayUpdateTitle {
		t.Errorf("menu title = %q, want %q", updateItem.Title(), trayUpdateTitle)
	}
}

// TestTrayControllerReportsUpdateFailure proves that a failing check reaches
// the user.
func TestTrayControllerReportsUpdateFailure(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, _, _ := testController(rec)

	ctl.opts.CheckUpdate = func(ctx context.Context) error { return errors.New("network down") }
	ctl.checkForUpdate()

	waitFor(t, "the failure report", func() bool { return rec.count() > 0 })
	if !strings.Contains(rec.text(), "network down") {
		t.Errorf("the failure should be reported, got %q", rec.text())
	}
}

// TestTrayControllerRemovesIconBeforeRequestingExit proves that the icon is
// gone before the application is asked to stop, so the notification area never
// keeps an entry for a dead process.
func TestTrayControllerRemovesIconBeforeRequestingExit(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, _, _ := testController(rec)

	var order []string
	ctl.quit = func() { order = append(order, "quit") }
	ctl.opts.RequestExit = func() { order = append(order, "exit") }

	ctl.requestExit()

	if len(order) != 2 || order[0] != "quit" || order[1] != "exit" {
		t.Errorf("order = %v, want the icon removed before the exit request", order)
	}
}

// TestTrayControllerOpensTheBoard proves that the menu entry opens the
// address the application is actually serving.
func TestTrayControllerOpensTheBoard(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, _, _ := testController(rec)

	var opened string
	ctl.open = func(url string) bool {
		opened = url
		return true
	}

	ctl.openBoard()
	if opened != "http://127.0.0.1:3333" {
		t.Errorf("opened %q, want the address of the running server", opened)
	}
}

// TestTrayControllerHandlesMenuClicks proves that the menu entries are wired
// to the actions.
func TestTrayControllerHandlesMenuClicks(t *testing.T) {
	rec := &notificationRecorder{}
	ctl, _, _ := testController(rec)

	openItem := newFakeMenuItem()
	exitItem := newFakeMenuItem()

	var openedMu sync.Mutex
	var opened string
	var quit atomic.Bool
	ctl.open = func(url string) bool {
		openedMu.Lock()
		defer openedMu.Unlock()
		opened = url
		return true
	}
	ctl.quit = func() { quit.Store(true) }

	ctl.run(openItem, exitItem)

	openItem.Activate()
	waitFor(t, "the board to open", func() bool {
		openedMu.Lock()
		defer openedMu.Unlock()
		return opened != ""
	})

	exitItem.Activate()
	waitFor(t, "the icon to be removed", quit.Load)
}
