//go:build !windows

// Automatic start outside Windows.
//
// Only Windows has a supported autostart mechanism (the Task Scheduler); the
// other platforms report it as unsupported, exactly like the installer does.
package main

// platformAutostartRegistration reports that nothing is registered: without a
// scheduler there is no registration to read.
func platformAutostartRegistration(dataDir string) (autostartRegistration, error) {
	return autostartRegistration{}, nil
}

// platformAutostartEnabled reports that automatic start is not in effect.
func platformAutostartEnabled(exe, dataDir string) (bool, error) {
	return false, nil
}

// platformAutostartApply refuses to change the autostart state: there is no
// mechanism to change it.
func platformAutostartApply(exe, dataDir string, enabled bool) error {
	return ErrAutostartUnsupported
}

// platformRestoreAutostart refuses to restore a registration: without a
// scheduler there is nothing to register.
func platformRestoreAutostart(dataDir, xml string) error {
	return ErrAutostartUnsupported
}
