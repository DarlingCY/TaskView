// Automatic start for the installed application.
//
// The state is always read from the operating system itself - the Task
// Scheduler on Windows - and never from the install manifest: the manifest
// records what the last install did, while the scheduler holds what is
// actually in effect. A user can switch the task off in Task Scheduler
// behind TaskView's back, and only the scheduler knows.
//
// Every function here acts on this installation's own registration. The
// scheduled task is named after a hash of the data directory, so another
// installation - or an unrelated task - is never read and never touched.
package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// autostartRegistration describes what the operating system currently holds
// for this installation's automatic start.
type autostartRegistration struct {
	// Exe is the executable the registered task launches. Empty when no task
	// is registered.
	Exe string

	// Args is the command line the registered task runs, already split into
	// its arguments.
	Args []string

	// Actions is the number of actions the registered task has.
	Actions int

	// Enabled is the task's own enabled flag, as the scheduler reports it.
	Enabled bool

	// Exists reports whether a task for this installation is registered at
	// all, even a disabled one.
	Exists bool

	// MatchesDataDir reports whether the registered command line starts
	// TaskView with this installation's data directory. The comparison is
	// exact: a data directory that is the prefix of another one does not
	// match.
	MatchesDataDir bool

	// XML is the registered task definition. It keeps a registration that is
	// switched off or that points somewhere else restorable.
	XML string
}

// Enables reports whether the registration would really start exe.
//
// Every condition has to hold: the task has to exist and be enabled, it must
// have exactly one action, that action has to launch this executable, and it
// has to start TaskView with this installation's data directory. A task that
// launches a different binary, or that starts TaskView with another data
// directory, is not this installation's autostart.
func (r autostartRegistration) Enables(exe string) bool {
	return r.Exists && r.Enabled && r.Actions == 1 &&
		samePath(r.Exe, exe) && r.MatchesDataDir
}

// matchesDataDir reports whether a registered command line starts TaskView
// with this data directory.
//
// The arguments are compared as arguments and never as text: a data directory
// that is the prefix of another one must not match, and a command line
// TaskView did not install must not match either.
func matchesDataDir(argv []string, dataDir string) bool {
	_, value, ok := parseLaunchForm(argv)
	if !ok {
		return false
	}
	return samePath(value, dataDir)
}

// launchForm names the ways a registered task can start TaskView.
type launchForm int

const (
	// launchUnknown is a command line TaskView did not install.
	launchUnknown launchForm = iota
	// launchBackground is the current form: --background --data-dir <dir>.
	launchBackground
	// launchServerStart is the form older installations used:
	// server start --port <n> --data-dir <dir>.
	launchServerStart
)

// parseLaunchForm recognises TaskView's launch forms and returns the data
// directory they use.
//
// The arguments are compared as arguments and never as text: a data directory
// that is the prefix of another one must not match.
func parseLaunchForm(argv []string) (launchForm, string, bool) {
	if len(argv) == 0 {
		return launchUnknown, "", false
	}

	var form launchForm
	switch argv[0] {
	case "--background":
		form = launchBackground
	case "server":
		if len(argv) < 2 || argv[1] != "start" {
			return launchUnknown, "", false
		}
		form = launchServerStart
	default:
		return launchUnknown, "", false
	}

	value, ok := switchValue(argv, "--data-dir")
	if !ok {
		return launchUnknown, "", false
	}
	return form, value, true
}

// switchValue returns the value of a switch, in both the "--switch value" and
// the "--switch=value" form.
func switchValue(argv []string, name string) (string, bool) {
	for i, arg := range argv {
		if arg == name {
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", false
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), true
		}
	}
	return "", false
}

// The platform halves of the autostart operations. They are variables so the
// tests can replace them: no test ever touches the real Task Scheduler.
var (
	autostartRegistrationReader = platformAutostartRegistration
	autostartStateReader        = platformAutostartEnabled
	autostartWriter             = platformAutostartApply
	autostartRestorer           = platformRestoreAutostart
)

// readAutostartRegistration reports what is registered for this installation.
//
// exe is the executable the registered task launches, enabled is the task's own
// enabled flag, and exists reports whether a task is registered at all.
//
// It deliberately does NOT take the install lock: the installer calls it while
// it already holds that lock, and the lock is not reentrant.
func readAutostartRegistration(dataDir string) (exe string, enabled bool, exists bool, err error) {
	reg, err := autostartRegistrationReader(dataDir)
	if err != nil {
		return "", false, false, err
	}
	return reg.Exe, reg.Enabled, reg.Exists, nil
}

// snapshotAutostart returns the registered task definition, or an empty string
// when no task is registered.
//
// The snapshot keeps a registration that is switched off or that points
// somewhere else restorable: registering it again with
// runTaskScheduler("create", scheduledTaskName(dataDir), xml) restores exactly
// what was there, including its enabled state.
//
// Like readAutostartRegistration it does not take the install lock.
func snapshotAutostart(dataDir string) (string, error) {
	reg, err := autostartRegistrationReader(dataDir)
	if err != nil {
		return "", err
	}
	if !reg.Exists {
		return "", nil
	}
	return reg.XML, nil
}

// restoreAutostart puts a previously captured task definition back.
//
// It is the counterpart of snapshotAutostart: the installer uses it to keep a
// registration that is switched off, or that points at an older executable,
// instead of dropping it.
//
// Like readAutostartRegistration it does not take the install lock: the
// installer calls it while it already holds that lock. The platform details -
// the scheduled task name in particular - stay behind this function, so a
// portable installer never has to know them.
func restoreAutostart(dataDir, xml string) error {
	return autostartRestorer(dataDir, xml)
}

// queryAutostart reports whether TaskView is registered to start automatically
// at logon with this executable and data directory.
//
// The answer comes from the platform, never from the install manifest.
//
// It takes the install lock, so it must not be called from a context that
// already holds it.
func queryAutostart(exe, dataDir string) (bool, error) {
	release, err := acquireInstallLock(filepath.Join(dataDir, installLockName), installLockTimeout)
	if err != nil {
		return false, fmt.Errorf("install lock: %w", err)
	}
	defer release()
	return autostartStateReader(exe, dataDir)
}

// setAutostart enables or disables the automatic start for this installation.
//
// Disabling removes only this instance's registration: the installed program,
// the data directory and every other setting stay untouched. It is not an
// uninstall.
//
// It takes the install lock, so it must not be called from a context that
// already holds it.
func setAutostart(exe, dataDir string, enabled bool) error {
	release, err := acquireInstallLock(filepath.Join(dataDir, installLockName), installLockTimeout)
	if err != nil {
		return fmt.Errorf("install lock: %w", err)
	}
	defer release()
	return autostartWriter(exe, dataDir, enabled)
}
