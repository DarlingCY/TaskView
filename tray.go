// Notification area icon.
//
// The tray is a thin shell around the running web UI: it opens the board in
// the browser, triggers the update check and manages the automatic start. It
// never owns the server and never touches the task database.
//
// The behaviour lives in trayController, which knows nothing about the
// notification area. The platform layer supplies the menu entries and the few
// side effects, so the whole controller can be exercised without creating a
// real icon.
package main

import (
	"context"
	"strings"
	"sync"
)

// trayOptions configures the notification area icon.
type trayOptions struct {
	// URL is the address of the local web UI. The "open board" action opens
	// exactly this address.
	URL string

	// DataDir is the directory that holds the TaskView database. It
	// identifies the installation whose automatic start is managed.
	DataDir string

	// RequestExit asks the application to shut down. The tray removes its
	// icon first, so the notification area never keeps an entry for a dead
	// process.
	RequestExit func()

	// CheckUpdate performs an update check. It runs off the UI thread and
	// may block; a request while another one is running is ignored. It
	// reports its own outcome to the user, so the tray only surfaces
	// failures.
	CheckUpdate func(context.Context) error

	// Context is the lifetime of the tray. The update check is bound to it,
	// so shutting TaskView down cancels a check that is still running.
	Context context.Context

	// Ready reports that the notification area icon is up and its menu can be
	// used. The application waits for it before it publishes its state, so a
	// health check can prove that the tray really started.
	//
	// It is called once, from the platform layer, as soon as the menu exists.
	// It must not block: the menu is built before the autostart state is
	// read, because reading the scheduler can take a moment.
	Ready func()
}

// Menu titles shared by the platform layer and the controller.
const (
	trayUpdateTitle        = "检查更新"
	trayUpdateRunningTitle = "正在检查…"
)

// clickable is a menu entry that reports activations.
type clickable interface {
	// Click returns the channel that receives a token each time the entry is
	// activated.
	Click() <-chan struct{}
}

// trayCheckItem is a checkable menu entry.
type trayCheckItem interface {
	clickable
	// SetChecked shows or clears the check mark.
	SetChecked(checked bool)
	// SetEnabled greys the entry out and stops it reacting to clicks.
	SetEnabled(enabled bool)
}

// trayActionItem is a plain menu entry.
type trayActionItem interface {
	clickable
	SetEnabled(enabled bool)
	SetTitle(title string)
}

// trayController implements the behaviour behind the tray menu.
//
// The side effects are fields rather than direct calls, so a test can drive
// the whole controller without a notification area, a browser or a scheduler.
type trayController struct {
	opts trayOptions

	exePath func() (string, error)
	query   func(exe, dataDir string) (bool, error)
	set     func(exe, dataDir string, enabled bool) error
	notify  func(title, message string)
	open    func(url string) bool
	quit    func()

	autostartItem trayCheckItem
	updateItem    trayActionItem

	// autostartMu serialises the autostart operations: the initial refresh
	// and a switch must never write the check mark at the same time.
	autostartMu sync.Mutex

	mu            sync.Mutex
	autostartBusy bool
	updateBusy    bool
}

// newTrayController wires the controller to the real platform operations.
func newTrayController(opts trayOptions, autostartItem trayCheckItem, updateItem trayActionItem) *trayController {
	return &trayController{
		opts:          opts,
		exePath:       executablePath,
		query:         queryAutostart,
		set:           setAutostart,
		notify:        platformNotify,
		open:          OpenBrowser,
		quit:          func() {},
		autostartItem: autostartItem,
		updateItem:    updateItem,
	}
}

// run connects the menu entries to the controller.
//
// Every entry gets its own goroutine, because the notification area only
// delivers a click to a reader that is already waiting for it.
func (c *trayController) run(open, exit trayActionItem) {
	go c.handle(open, c.openBoard)
	go c.handle(exit, c.requestExit)
	go c.handle(c.autostartItem, c.toggleAutostart)
	go c.handle(c.updateItem, c.checkForUpdate)

	// The real state is read in the background: the scheduler can take a
	// moment, and the icon must not wait for it.
	go c.refreshAutostart()
}

// handle waits for activations of an entry and runs the action for each one.
func (c *trayController) handle(item clickable, action func()) {
	if item == nil {
		return
	}
	for range item.Click() {
		action()
	}
}

// openBoard opens the web UI in the default browser.
func (c *trayController) openBoard() {
	url := strings.TrimSpace(c.opts.URL)
	if url == "" {
		return
	}
	if c.open != nil && !c.open(url) {
		c.notify("TaskView", "无法打开浏览器，请手动访问 "+url)
	}
}

// requestExit removes the icon and asks the application to stop.
func (c *trayController) requestExit() {
	// The icon goes first: the application may shut down immediately
	// afterwards, and a leftover icon would point at a dead process.
	if c.quit != nil {
		c.quit()
	}
	if c.opts.RequestExit != nil {
		c.opts.RequestExit()
	}
}

// refreshAutostart asks the platform for the real state and shows it.
//
// It shares the autostart lock with the switch, so the check mark is never
// written by two operations at once, and it does not block a click: a switch
// that arrives while the refresh runs waits for it instead of being lost.
func (c *trayController) refreshAutostart() {
	c.autostartMu.Lock()
	defer c.autostartMu.Unlock()
	if err := c.syncAutostartState(); err != nil {
		c.notify("TaskView", "无法读取开机自启状态: "+err.Error())
	}
}

// syncAutostartState reads the real state and puts it into the menu.
//
// The caller holds the autostart lock.
func (c *trayController) syncAutostartState() error {
	exe, err := c.exePath()
	if err != nil {
		return err
	}
	enabled, err := c.query(exe, c.opts.DataDir)
	if err != nil {
		return err
	}
	c.setAutostartChecked(enabled)
	return nil
}

// toggleAutostart flips the automatic start.
//
// The scheduler call takes seconds, so it runs off the UI thread. The state
// is read from the platform before the switch, not taken from the menu: the
// task can be switched off outside TaskView, and the click has to mean what
// the user expects it to mean. The check mark always ends up showing the real
// state, and after a failure the platform is asked again instead of leaving
// the menu showing what was asked for.
func (c *trayController) toggleAutostart() {
	if !c.beginAutostart() {
		// A switch is already running. A second click would fight with it.
		return
	}
	go func() {
		defer c.endAutostart()
		if c.autostartItem != nil {
			c.autostartItem.SetEnabled(false)
			defer c.autostartItem.SetEnabled(true)
		}

		c.autostartMu.Lock()
		defer c.autostartMu.Unlock()

		exe, err := c.exePath()
		if err != nil {
			c.notify("TaskView", "无法确定程序路径: "+err.Error())
			return
		}
		current, err := c.query(exe, c.opts.DataDir)
		if err != nil {
			c.notify("TaskView", "无法读取开机自启状态: "+err.Error())
			return
		}
		desired := !current
		if err := c.set(exe, c.opts.DataDir, desired); err != nil {
			c.notify("TaskView", "开机自启设置失败: "+err.Error())
			// Show what is really registered, not what was asked for.
			if syncErr := c.syncAutostartState(); syncErr != nil {
				c.notify("TaskView", "无法读取开机自启状态: "+syncErr.Error())
			}
			return
		}
		c.setAutostartChecked(desired)
	}()
}

// checkForUpdate runs the update check off the UI thread.
//
// A second click while a check is running is ignored, so a slow check cannot
// be started twice.
func (c *trayController) checkForUpdate() {
	if c.opts.CheckUpdate == nil {
		return
	}
	if !c.beginUpdate() {
		return
	}
	go func() {
		defer c.endUpdate()
		if c.updateItem != nil {
			c.updateItem.SetTitle(trayUpdateRunningTitle)
			c.updateItem.SetEnabled(false)
			defer func() {
				c.updateItem.SetTitle(trayUpdateTitle)
				c.updateItem.SetEnabled(true)
			}()
		}

		ctx, cancel := c.updateContext()
		defer cancel()
		if err := c.opts.CheckUpdate(ctx); err != nil {
			c.notify("TaskView", "检查更新失败: "+err.Error())
		}
	}()
}

// updateContext returns the context an update check runs under.
//
// The check is bound to the lifetime of the tray and nothing else: it carries
// no deadline of its own. The update divides itself into phases and gives
// every phase its own budget, and a deadline here would spend that budget
// while the user is still reading the confirmation prompt.
func (c *trayController) updateContext() (context.Context, context.CancelFunc) {
	parent := c.opts.Context
	if parent == nil {
		parent = context.Background()
	}
	return context.WithCancel(parent)
}

// beginAutostart reserves the autostart action. It reports false when an
// action is already running.
func (c *trayController) beginAutostart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.autostartBusy {
		return false
	}
	c.autostartBusy = true
	return true
}

// endAutostart releases the autostart action.
func (c *trayController) endAutostart() {
	c.mu.Lock()
	c.autostartBusy = false
	c.mu.Unlock()
}

// beginUpdate reserves the update action. It reports false when an action is
// already running.
func (c *trayController) beginUpdate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateBusy {
		return false
	}
	c.updateBusy = true
	return true
}

// endUpdate releases the update action.
func (c *trayController) endUpdate() {
	c.mu.Lock()
	c.updateBusy = false
	c.mu.Unlock()
}

// setAutostartChecked shows the state in the menu.
func (c *trayController) setAutostartChecked(checked bool) {
	if c.autostartItem != nil {
		c.autostartItem.SetChecked(checked)
	}
}
