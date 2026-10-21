// TaskView installed-application support.
//
// The public interface is the installed tray application plus its local HTTP
// API; there is no command line interface. Install and Uninstall remain the
// library-level primitives behind the installer helper. Nothing here runs as a
// side effect of any other entry point, PATH is never modified, and the task
// database is never touched.
//
// Windows registers an optional login autostart through Task Scheduler (not the
// Windows Service Control Manager), scoped to the current interactive user at
// least privilege with no password and no elevation. Every other platform
// supports install/uninstall but reports autostart as unsupported before
// anything is written to disk.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrAutostartUnsupported is returned by Install when Autostart is requested on
// a platform without a supported autostart mechanism. It is raised before any
// filesystem mutation, so a rejected request leaves the system untouched.
var ErrAutostartUnsupported = errors.New("autostart is only supported on Windows; install without --autostart")

// installLockTimeout bounds how long Install/Uninstall wait for the cross
// process lock before giving up.
const installLockTimeout = 10 * time.Second

// installManifestName is the manifest file inside the data directory.
const installManifestName = "install.json"

// installLockName is the cross process lock inside the data directory.
const installLockName = "install.lock"

// InstallOptions controls an install run.
//
// Autostart is opt-in and defaults to false. It has no "disable" value: once a
// user has opted in, a later plain Install keeps the registration, and Uninstall
// is the way to remove it.
type InstallOptions struct {
	Autostart bool
}

// InstallResult reports what Install actually did.
//
// Path is the canonical managed executable that now exists on disk.
// Autostart is the registration state that is genuinely in effect; it is only
// true after the scheduler operation succeeded.
type InstallResult struct {
	Path      string
	Autostart bool
}

// installManifest records the installed location and autostart state.
//
// Path is informational only. Uninstall deliberately ignores it and re-derives
// the managed path from DataDir instead, so a tampered or stale manifest can
// never redirect deletion at an arbitrary file.
type installManifest struct {
	Path        string `json:"path"`
	Autostart   bool   `json:"autostart"`
	InstalledAt string `json:"installed_at"`
	Version     string `json:"version,omitempty"`
}

// ---------------------------------------------------------------------------
// Injection seams.
//
// Tests override these to exercise the lifecycle without touching a real
// scheduler, a real database or the real running executable.
// ---------------------------------------------------------------------------

// executablePath is the portable binary being installed. Tests point this at a
// throwaway file so the lifecycle can be driven without the real binary.
var executablePath = os.Executable

// runCommand executes an external command. Tests point this at a throwaway
// runner so nothing touches the real machine.
var runCommand = defaultRunCommand

// runTaskScheduler performs the platform's autostart registration step. Tests
// override it so no real scheduled task is ever created.
//
// The default delegates to the Task Scheduler COM call on Windows; other
// platforms have none, because they reject --autostart before reaching it.
var runTaskScheduler = func(mode, taskName, xmlDoc string) error {
	return platformRunTaskScheduler(mode, taskName, xmlDoc)
}

// validateTaskDefinition checks a generated task document before it reaches the
// scheduler, so a structural mistake surfaces here with a useful message rather
// than as an opaque COM error at registration time.
//
// The Windows implementation performs local checks: non-empty, well-formed XML
// with a Task root. It deliberately does NOT assert that every element name
// exists in the TaskSettings schema - a wrong element is still reported by the
// scheduler at registration, just sooner than it would be without this seam.
//
// The default is a no-op, which is correct for platforms without a scheduler.
var validateTaskDefinition = func(xmlDoc string) error {
	return platformValidateTaskDefinition(xmlDoc)
}

// acquireInstallLock serialises install/uninstall across processes against
// <data-dir>/install.lock. The default is the daemon's OS-held process lock,
// shared with the server so both subsystems serialise against each other.
var acquireInstallLock = acquireProcessLock

// selfDeletePending records the path Uninstall could not remove inline because
// it is the currently running executable.
var selfDeletePending = struct {
	mu   sync.Mutex
	path string
}{}

func defaultRunCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String() + stderr.String(),
			fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stdout.String()+stderr.String()))
	}
	return stdout.String() + stderr.String(), nil
}

// SelfDeleteScheduled returns the managed executable path when the most recent
// Uninstall had to defer deletion because that file is the running process.
//
// Optional for the CLI writer: Uninstall still returns nil in that case
// because the removal is reliably queued, and this only exists so the deferral
// can be surfaced honestly instead of silently orphaning a file.
func SelfDeleteScheduled() string {
	selfDeletePending.mu.Lock()
	defer selfDeletePending.mu.Unlock()
	return selfDeletePending.path
}

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

// installTargetPath returns the canonical managed location for the installed
// binary: <data-dir>/bin/taskview.exe on Windows, <data-dir>/bin/taskview
// elsewhere.
func installTargetPath(dataDir string) (string, error) {
	name, err := platformBinaryName()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "bin", name), nil
}

// installManifestPath returns <data-dir>/install.json.
func installManifestPath(dataDir string) string {
	return filepath.Join(dataDir, installManifestName)
}

// runningManagedBinary reports whether the current process *is* the managed
// executable. Uninstall uses it to pick between direct removal and deferred
// removal, and Install uses it to explain a failed replacement.
func runningManagedBinary(managed string) bool {
	self, err := executablePath()
	if err != nil {
		return false
	}
	return samePath(self, managed)
}

// samePath compares two paths after making them absolute and clean. On Windows
// the comparison is case-insensitive; elsewhere it is exact.
// samePath compares two paths after making them absolute and clean.
//
// Only Windows folds case, because only Windows guarantees a case-insensitive
// filesystem. Folding elsewhere would report two genuinely distinct POSIX
// paths as the same file, which is the same class of bug this comparison exists
// to avoid.
func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	cleanA, cleanB := filepath.Clean(absA), filepath.Clean(absB)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(cleanA, cleanB)
	}
	return cleanA == cleanB
}

// ---------------------------------------------------------------------------
// Install
// ---------------------------------------------------------------------------

// Install copies the running executable to the managed location and, when
// Autostart is requested on Windows, registers a Task Scheduler logon task.
//
// It never modifies PATH, never starts the server and never touches the task
// database. Repeating an install is idempotent.
//
// On failure the returned *InstallResult may still be non-nil: when only the
// autostart registration failed, the executable is installed and runnable and
// the error explains exactly what to do about the registration. Autostart is
// false in that case, and the manifest never records an unconfirmed
// registration.
func Install(opts InstallOptions) (*InstallResult, error) {
	// Reject an unsupported request before creating directories or files.
	if opts.Autostart && !platformAutostartSupported() {
		return nil, ErrAutostartUnsupported
	}

	dataDir, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}

	managed, err := installTargetPath(dataDir)
	if err != nil {
		return nil, err
	}

	release, err := acquireInstallLock(filepath.Join(dataDir, installLockName), installLockTimeout)
	if err != nil {
		return nil, fmt.Errorf("install lock: %w", err)
	}
	defer release()

	serverRelease, err := stopManagedServerForInstall()
	if err != nil {
		return nil, fmt.Errorf("stop the TaskView server before updating: %w", err)
	}
	if serverRelease != nil {
		defer serverRelease()
	}

	previous, err := readInstallManifest(dataDir)
	if err != nil {
		return nil, err
	}

	source, err := executablePath()
	if err != nil {
		return nil, fmt.Errorf("locate running executable: %w", err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return nil, fmt.Errorf("locate running executable: %w", err)
	}

	identical, err := filesHaveSameContents(source, managed)
	if err != nil {
		return nil, fmt.Errorf("compare executable: %w", err)
	}

	if !identical {
		if err := replaceExecutable(source, managed); err != nil {
			return nil, err
		}
	}

	// A prior explicit opt-in survives a plain reinstall; only Uninstall removes
	// the registration.
	wantAutostart := opts.Autostart || previous.Autostart

	result := &InstallResult{Path: managed}

	if wantAutostart && platformAutostartSupported() {
		if err := platformRegisterAutostart(managed, dataDir); err != nil {
			// The executable is installed and runnable. Record the honest
			// state (not registered) and hand back an actionable error.
			_ = writeInstallManifest(dataDir, &installManifest{Path: managed, Autostart: false})
			result.Autostart = false
			return result, err
		}
		result.Autostart = true
	}

	if err := writeInstallManifest(dataDir, &installManifest{
		Path:        managed,
		Autostart:   result.Autostart,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
		Version:     appVersion(),
	}); err != nil {
		return result, fmt.Errorf("installed %s but could not write manifest: %w", managed, err)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Uninstall
// ---------------------------------------------------------------------------

// Uninstall removes the autostart registration, the install manifest and the
// managed executable.
//
// It only ever acts on the path it derives itself from DataDir, so a tampered
// manifest cannot redirect it. It never deletes the data directory, the task
// database or any file it did not create, and it is safe to repeat.
func Uninstall() error {
	// Clear the pending marker up front. If this run cannot delete the
	// executable inline it is set again by scheduleSelfDelete, and clearing it
	// afterwards would wipe the very state the caller needs in order to report
	// that the removal was deferred.
	selfDeletePending.mu.Lock()
	selfDeletePending.path = ""
	selfDeletePending.mu.Unlock()

	dataDir, err := DataDir()
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}

	managed, err := installTargetPath(dataDir)
	if err != nil {
		return err
	}

	release, err := acquireInstallLock(filepath.Join(dataDir, installLockName), installLockTimeout)
	if err != nil {
		return fmt.Errorf("install lock: %w", err)
	}
	defer release()

	serverRelease, err := stopManagedServerForInstall()
	if err != nil {
		return fmt.Errorf("stop the TaskView server: %w", err)
	}
	if serverRelease != nil {
		defer serverRelease()
	}

	// Removal is idempotent: a task that was never registered is not an error.
	if err := platformRemoveAutostart(dataDir); err != nil {
		return err
	}

	if err := os.Remove(installManifestPath(dataDir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove install manifest: %w", err)
	}

	if err := platformRemoveInstalled(managed); err != nil {
		return err
	}

	// The pending marker is deliberately NOT cleared here. It is cleared at the
	// top of Uninstall and set again by scheduleSelfDelete when removal had to be
	// deferred; clearing it now would erase exactly the state the caller needs in
	// order to report that the file will disappear only after this process exits.
	return nil
}

// ---------------------------------------------------------------------------
// File helpers
// ---------------------------------------------------------------------------

// replaceExecutable copies src over dst atomically: the bytes land in a
// temporary file in the destination directory, are flushed, and are then moved
// into place. A failure part way through leaves the previous binary intact.
func replaceExecutable(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source executable %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, ".taskview-install-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup; a successful rename makes this a no-op.
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return fmt.Errorf("copy executable: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flush executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close executable: %w", err)
	}

	if err := platformPostCopy(tmpName); err != nil {
		return err
	}

	if err := platformReplaceExecutable(tmpName, dst); err != nil {
		return fmt.Errorf("replace %s: %w%s", dst, err, runningBinaryHint(dst))
	}
	return nil
}

// runningBinaryHint adds advice for the common failure mode: the destination is
// currently executing, so the OS refuses to replace it.
func runningBinaryHint(dst string) string {
	if runningManagedBinary(dst) {
		return ""
	}
	return " (is TaskView still running? stop it and retry)"
}

// filesHaveSameContents reports whether src and dst already hold identical
// bytes. A missing destination is never identical.
func filesHaveSameContents(src, dst string) (bool, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if srcInfo.Size() != dstInfo.Size() {
		return false, nil
	}

	srcSum, err := fileDigest(src)
	if err != nil {
		return false, err
	}
	dstSum, err := fileDigest(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return srcSum == dstSum, nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------

// readInstallManifest loads the manifest.
//
// A missing manifest is normal and returns the zero value. Any other read error
// or malformed JSON is reported, because silently treating an unreadable
// manifest as "no autostart" would discard a user's explicit opt-in on the very
// install that should honour it.
func readInstallManifest(dataDir string) (installManifest, error) {
	var manifest installManifest

	raw, err := os.ReadFile(installManifestPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return manifest, nil
		}
		return manifest, fmt.Errorf("read install manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("install manifest %s is malformed: %w", installManifestPath(dataDir), err)
	}
	return manifest, nil
}

// writeInstallManifest writes the manifest atomically with owner-only
// permissions: the bytes land in a temp file in the same directory and are
// renamed into place, so a reader never sees a partial document.
func writeInstallManifest(dataDir string, manifest *installManifest) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	path := installManifestPath(dataDir)

	tmp, err := os.CreateTemp(dataDir, ".install-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return platformProtectFile(path)
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Deferred self-removal
//
// Only Windows needs this: Windows refuses to delete a running image, while on
// POSIX unlinking a running binary simply succeeds. The helper and its script
// therefore live in install_windows.go.
// ---------------------------------------------------------------------------
