//go:build windows

// Windows install support.
//
// Autostart uses Task Scheduler (never the Service Control Manager) via COM, so
// no administrator rights, service account or stored password are involved: the
// task runs as the interactive user who registered it, at logon, with least
// privilege.
//
// COM rather than schtasks because schtasks output is localised, and this
// machine runs a Chinese locale where English message matching is useless. COM
// returns a numeric HRESULT instead.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const windowsBinaryName = "taskview.exe"
const taskServerPort = 3333

// taskRunDuration is unlimited; the TaskSettings default of PT72H would silently
// kill the server after three days.
const taskRunDuration = "PT0S"

// taskRunTimeout bounds register/remove so a wedged COM call cannot hang install.
const taskRunTimeout = 15 * time.Second

// platformAutostartDefinition renders the logon task for this installation
// without registering it.
//
// It is used to carry an explicitly disabled registration over to a new
// executable: the registration must keep pointing at the program that is now
// installed, and it must stay disabled.
func platformAutostartDefinition(managed, dataDir string, enabled bool) (string, error) {
	return taskXMLFor(managed, dataDir, scheduledTaskName(dataDir), enabled)
}

// platformAutostartSupported reports whether this platform can register a
// login item.
func platformAutostartSupported() bool { return true }

func platformBinaryName() (string, error) { return windowsBinaryName, nil }

// platformPostCopy is a no-op: Windows has no POSIX mode bits and execute rights
// come from the parent directory.
func platformPostCopy(path string) error { return nil }

// platformProtectFile is a no-op on Windows.
func platformProtectFile(path string) error { return nil }

// currentUserIdentity returns the Windows SID of the user running this process.
// os/user reaches this through the Windows API (NetUserGetInfo /
// LookupAccountSid), unlike USERNAME/COMPUTERNAME which are per-process
// environment values and can be absent or stale.
func currentUserIdentity() (sid, name string) {
	if u, err := user.Current(); err == nil && u != nil {
		sid = u.Uid
		if u.Username != "" {
			return sid, u.Username
		}
	}
	// Fall back to env only if the API path is unavailable.
	domain, usr := os.Getenv("USERDOMAIN"), os.Getenv("USERNAME")
	if domain == "" {
		domain = os.Getenv("COMPUTERNAME")
	}
	return "", domain + `\` + usr
}

// scheduledTaskName derives a stable task name from the user SID and data dir.
//
// Hashing both the SID and a lowercased data directory isolates concurrent
// TASKVIEW_HOME overrides from one another and from any unrelated task, while
// staying deterministic so a repeat install updates one task instead of
// accumulating duplicates. Lowercasing matches Windows path semantics, so the
// same directory always yields the same task.
func scheduledTaskName(dataDir string) string {
	sid, name := currentUserIdentity()
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		absDir = dataDir
	}

	h := sha256.New()
	fmt.Fprintf(h, "sid=%s\x00name=%s\x00dataDir=%s\x00port=%d\x00",
		sid, name, strings.ToLower(filepath.Clean(absDir)), taskServerPort)
	return "TaskView-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// scheduledTaskCommand returns the arguments for the autostarted server.
// --data-dir is absolute so the task finds the right database despite Task
// Scheduler's %SystemRoot%\System32 working directory.
func scheduledTaskCommand(managed, dataDir string) []string {
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		absDir = dataDir
	}
	return []string{"--background", "--data-dir", filepath.Clean(absDir)}
}

// composeTaskArguments renders the Exec action's <Arguments>.
//
// The executable path belongs in <Command>, so argv[0] must NOT be repeated
// here: including it makes the task run "C:\...\taskview.exe" as the first
// command word, which TaskView would reject as an unknown subcommand.
func composeTaskArguments(args []string) string {
	return windows.ComposeCommandLine(args)
}

// taskXML builds an enabled task definition.
func taskXML(managed, dataDir, taskName string) (string, error) {
	return taskXMLFor(managed, dataDir, taskName, true)
}

// taskXMLFor builds the task definition. Task Scheduler XML is embedded in a
// command, so every interpolated value is XML-escaped. A disabled definition
// keeps a registration visible in the scheduler without starting anything.
func taskXMLFor(managed, dataDir, taskName string, enabled bool) (string, error) {
	enabledText := "true"
	if !enabled {
		enabledText = "false"
	}
	_, name := currentUserIdentity()
	if name == "" {
		name = escapeXML(taskName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = dataDir
	}

	// Settings are kept to properties that actually exist in TaskSettings.
	// Hidden only hides the task from the Task Scheduler UI; it does not hide
	// the console window of the Exec action, so a brief window may appear.
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>TaskView local web UI (autostart at logon)</Description>
    <URI>\%s</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>%s</Enabled>
      <UserId>%s</UserId>
      <Delay>PT10S</Delay>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>%s</Enabled>
    <Hidden>true</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>%s</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`,
		escapeXML(taskName),
		enabledText,
		escapeXML(name),
		escapeXML(name),
		enabledText,
		taskRunDuration,
		escapeXML(managed),
		escapeXML(composeTaskArguments(scheduledTaskCommand(managed, dataDir))),
		escapeXML(home),
	), nil
}

func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ---------------------------------------------------------------------------
// Autostart registration via Task Scheduler COM
// ---------------------------------------------------------------------------

// errTaskAbsent is returned when the scheduled task does not exist. Uninstall
// treats it as success.
var errTaskAbsent = errors.New("scheduled task not found")

// comScript is embedded in an encoded PowerShell payload. It talks to
// ITaskService directly and reports status through exit codes, so no human
// readable (localised) message has to be parsed.
//
//	0 = ok, 1 = the task does not exist, 2 = any other failure
//
// TASK_DEFINITION_FILE holds the path to the XML to register, so stdin carries
// only the task name.
//
// Every path ends in an explicit exit: falling through would otherwise run the
// registration branch during a removal.
const comScript = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$taskName = [Console]::In.ReadLine()
$mode = $env:TASKVIEW_COM_MODE

# HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND) == -2147024894 (0x80070002).
# GetTask THROWS this for a missing task rather than returning null, so it has
# to be unwrapped out of the MethodInvocationException. Anything else - notably
# access denied (-2147024891) - must propagate as a real failure.
$HR_NOT_FOUND = -2147024894

function Unwrap($e) {
  while ($null -ne $e -and $null -ne $e.InnerException) { $e = $e.InnerException }
  return $e
}

try {
  $svc = New-Object -ComObject 'Schedule.Service'
  $svc.Connect()
  $root = $svc.GetFolder('\')

  if ($mode -eq 'remove') {
    try {
      $root.GetTask($taskName) | Out-Null
    } catch {
      $inner = Unwrap $_.Exception
      if ($inner.HResult -eq $HR_NOT_FOUND) { exit 1 }
      throw
    }
    $root.DeleteTask($taskName, 0)
    exit 0
  }

  $xml = [System.IO.File]::ReadAllText($env:TASKVIEW_TASK_XML, [System.Text.Encoding]::UTF8)
  $xml = $xml -replace '^\s*<\?xml[^?]*\?>\s*', ''
  $root.RegisterTask($taskName, $xml, 6, '', '', 3, '') | Out-Null
  exit 0
} catch {
  [Console]::Error.WriteLine($_.Exception.Message)
  exit 2
}
`

// platformValidateTaskDefinition performs local checks on a generated task
// document. No task is created or changed.
//
// Scope is deliberately narrow and stated honestly: empty documents, malformed
// XML and a wrong root element are rejected here, so those mistakes never reach
// the scheduler. Element-name validity against the full TaskSettings schema is
// not checked - that requires either the installed XSD or a real registration,
// and the scheduler already rejects an unknown element with a clear error.
func platformValidateTaskDefinition(xmlDoc string) error {
	trimmed := strings.TrimSpace(xmlDoc)
	if trimmed == "" {
		return errors.New("empty task definition")
	}

	// Parse the whole document, not just the first element: stopping at the root
	// would report `<?xml?><Task><unclosed>` as well-formed.
	var doc struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(trimmed), &doc); err != nil {
		return fmt.Errorf("task definition is not well-formed XML: %w", err)
	}
	if doc.XMLName.Local != "Task" {
		return fmt.Errorf("task definition root element is <%s>, want <Task>", doc.XMLName.Local)
	}

	// Reject an encoding declaration that contradicts the UTF-8 bytes written.
	if decl := firstLine(trimmed); !strings.Contains(decl, `encoding="UTF-8"`) {
		return fmt.Errorf("task definition must declare encoding=\"UTF-8\", got %q", decl)
	}
	return nil
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// taskDefinitionFile writes xmlDoc to a temp file and returns its path.
// Task Scheduler reads the definition as UTF-8, matching the declaration.
func taskDefinitionFile(xmlDoc string) (string, error) {
	f, err := os.CreateTemp("", "taskview-task-*.xml")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.WriteString(xmlDoc); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// platformRunTaskScheduler is the platform half of the runTaskScheduler seam.
// It maps PowerShell exit codes to errors; errTaskAbsent means "not registered".
func platformRunTaskScheduler(mode, taskName, xmlDoc string) error {
	ctx, cancel := context.WithTimeout(context.Background(), taskRunTimeout)
	defer cancel()

	script := comScript
	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(script)))

	cmd := exec.CommandContext(ctx, powershellPath(),
		"-NoProfile", "-NonInteractive", "-OutputFormat", "Text",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encoded)
	cmd.Env = append(os.Environ(), "TASKVIEW_COM_MODE="+mode)
	cmd.Stdin = strings.NewReader(taskName + "\n")

	if mode == "create" {
		path, err := taskDefinitionFile(xmlDoc)
		if err != nil {
			return fmt.Errorf("write task definition file: %w", err)
		}
		defer os.Remove(path)
		cmd.Env = append(cmd.Env, "TASKVIEW_TASK_XML="+path)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	err := cmd.Run()
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 1:
			return errTaskAbsent
		case 2:
			return fmt.Errorf("task scheduler rejected %s: %w: %s",
				mode, err, strings.TrimSpace(stderr.String()))
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("task scheduler %s timed out after %s", mode, taskRunTimeout)
	}
	return fmt.Errorf("task scheduler %s failed: %w: %s", mode, err, strings.TrimSpace(stderr.String()))
}

func platformRegisterAutostart(managed, dataDir string) error {
	taskName := scheduledTaskName(dataDir)

	xmlDoc, err := taskXML(managed, dataDir, taskName)
	if err != nil {
		return fmt.Errorf("build scheduled task definition: %w", err)
	}
	// Catch a bad schema before touching the scheduler, where the failure would
	// be an opaque COM error.
	if err := validateTaskDefinition(xmlDoc); err != nil {
		return fmt.Errorf("generated task definition is invalid: %w", err)
	}
	if err := runTaskScheduler("create", taskName, xmlDoc); err != nil {
		return autostartRegistrationError(err, taskName)
	}
	return nil
}

func platformRemoveAutostart(dataDir string) error {
	taskName := scheduledTaskName(dataDir)

	err := runTaskScheduler("remove", taskName, "")
	switch {
	case err == nil, errors.Is(err, errTaskAbsent):
		return nil
	default:
		return fmt.Errorf("could not remove the TaskView logon task %q: %w\n"+
			"remove it manually in Task Scheduler, or with:\n"+
			"  schtasks /Delete /TN %s /F", taskName, err, taskName)
	}
}

// autostartRegistrationError adds guidance. Task creation is often refused for
// standard users by local policy; TaskView never elevates itself.
func autostartRegistrationError(err error, taskName string) error {
	return fmt.Errorf("could not register the TaskView logon task %s: %w\n"+
		"open TaskView from the Start menu and turn the autostart switch on again; "+
		"no automatic elevation",
		taskName, err)
}

// ---------------------------------------------------------------------------
// Executable removal
// ---------------------------------------------------------------------------

// platformReplaceExecutable renames src to dst, retrying on sharing/access denied
// while Windows finishes unmapping an exiting server executable image.
func platformReplaceExecutable(src, dst string) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := os.Rename(src, dst)
		if err == nil {
			return nil
		}
		if !isBinaryInUseError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// platformRemoveInstalled deletes the managed executable.
//
// When it is the running process Windows refuses with a sharing violation, so
// removal is deferred to a helper that runs after this process exits.
// External callers retry up to 3 seconds for in-use errors before failing.
func platformRemoveInstalled(managed string) error {
	err := os.Remove(managed)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if !isBinaryInUseError(err) {
		return fmt.Errorf("remove %s: %w", managed, err)
	}
	if runningManagedBinary(managed) {
		return scheduleSelfDelete(managed)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		err = os.Remove(managed)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if !isBinaryInUseError(err) {
			return fmt.Errorf("remove %s: %w", managed, err)
		}
	}
	return fmt.Errorf("remove %s: %w", managed, err)
}

// isBinaryInUseError reports whether the OS refused the delete because the file
// is mapped as a running image, using numeric error codes rather than
// localised message text.
func isBinaryInUseError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// ---------------------------------------------------------------------------
// Deferred self-removal
// ---------------------------------------------------------------------------

// selfDeleteGuard identifies the file a deferred-removal helper may delete, and
// the two conditions that prove it is still the copy being uninstalled.
//
// The digest alone is not enough: a reinstall that writes identical bytes
// produces the same hash, so a hash match cannot distinguish the old copy from
// a fresh replacement. The manifest check closes that gap - Uninstall removes
// install.json before scheduling, so a manifest present later means an install
// completed in the meantime and the file must be left alone.
type selfDeleteGuard struct {
	Path     string // exact managed executable path
	SHA256   string // digest at schedule time
	LockFile string // <data-dir>/install.lock, the byte lock install/uninstall holds
	Manifest string // <data-dir>/install.json, removed by uninstall before scheduling
}

// scheduleSelfDelete queues removal of path after this process exits.
//
// The helper waits for this process, takes the same exclusive byte lock on
// install.lock that Install and Uninstall hold, and deletes the file only if no
// manifest has reappeared and the digest still matches.
func scheduleSelfDelete(path string) error {
	sum, err := fileDigest(path)
	if err != nil {
		return fmt.Errorf("cannot guard deferred removal of %s: %w", path, err)
	}

	dataDir, err := DataDir()
	if err != nil {
		return fmt.Errorf("resolve data directory for deferred removal: %w", err)
	}
	// Absolute, because the helper may inherit an unexpected working directory.
	if abs, absErr := filepath.Abs(dataDir); absErr == nil {
		dataDir = abs
	}

	selfDeletePending.mu.Lock()
	selfDeletePending.path = path
	selfDeletePending.mu.Unlock()

	guard := selfDeleteGuard{
		Path:     path,
		SHA256:   sum,
		LockFile: filepath.Join(dataDir, installLockName),
		Manifest: installManifestPath(dataDir),
	}
	script := selfDeleteScript(guard)
	if err := runSelfDeleteHelper(script, os.Getpid()); err != nil {
		selfDeletePending.mu.Lock()
		selfDeletePending.path = ""
		selfDeletePending.mu.Unlock()
		return fmt.Errorf("could not schedule removal of %s: %w\n"+
			"the file is still in place; delete it manually once TaskView has exited", path, err)
	}
	return nil
}

// selfDeleteScript waits on the owning process, then removes path only while it
// still matches the expected hash. Both waits are deadline bounded so the helper
// can never spin forever.
func selfDeleteScript(guard selfDeleteGuard) string {
	return fmt.Sprintf(
		"$ErrorActionPreference='SilentlyContinue';"+
			"$env:PSModulePath=\"$env:SystemRoot\\system32\\WindowsPowerShell\\v1.0\\Modules\";"+
			"$f=%s;$want=%s;$lock=%s;$manifest=%s;"+
			"$owner=[int]$env:TASKVIEW_OWNER_PID;"+
			// Wait for the owning process, bounded.
			"$deadline=(Get-Date).AddSeconds(30);"+
			"while((Get-Date) -lt $deadline){"+
			"if(-not (Get-Process -Id $owner -ErrorAction SilentlyContinue)){break};"+
			"Start-Sleep -Milliseconds 250};"+
			// Take an exclusive lock on install.lock so an install cannot be
			// midway through replacing the binary. This is stricter than the
			// Go side's LockFileEx byte range: FileShare::None denies any other
			// opener at all, which also serialises with os.Open.
			"$lockHandle=$null;"+
			"$lockDeadline=(Get-Date).AddSeconds(10);"+
			"while(($null -eq $lockHandle) -and ((Get-Date) -lt $lockDeadline)){"+
			"try{"+
			"$lockHandle=[System.IO.File]::Open($lock,[System.IO.FileMode]::OpenOrCreate,"+
			"[System.IO.FileAccess]::ReadWrite,[System.IO.FileShare]::None);"+
			"break"+
			"}catch{Start-Sleep -Milliseconds 250}"+
			"};"+
			// Never delete without the lock: a concurrent install owns the file.
			"if($null -eq $lockHandle){exit 0};"+
			"$deadline=(Get-Date).AddSeconds(10);"+
			"while((Get-Date) -lt $deadline){"+
			// A reinstall recreated the manifest: leave the file in place.
			"if(Test-Path -LiteralPath $manifest){break};"+
			"if(-not (Test-Path -LiteralPath $f)){break};"+
			"$h=(Get-FileHash -LiteralPath $f -Algorithm SHA256 -ErrorAction SilentlyContinue).Hash;"+
			// The trailing semicolon is required: without it PowerShell reports
			// "unexpected token" at the following Remove-Item.
			"if($h -ne $want){break};"+
			"Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue;"+
			"if(-not (Test-Path -LiteralPath $f)){break};"+
			"Start-Sleep -Milliseconds 250"+
			"};"+
			"$lockHandle.Dispose()",
		quoteForPowerShell(guard.Path),
		quoteForPowerShell(guard.SHA256),
		quoteForPowerShell(guard.LockFile),
		quoteForPowerShell(guard.Manifest),
	)
}

// quoteForPowerShell renders a single-quoted PowerShell literal.
func quoteForPowerShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// runSelfDeleteHelper launches the detached helper.
func runSelfDeleteHelper(script string, ownerPID int) error {
	// The owning PID is passed through the environment, not interpolated into
	// the script: a placeholder substituted into a $-prefixed token would yield
	// something like "$123", which is not valid PowerShell.
	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(script)))

	cmd := exec.Command(powershellPath(), "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", encoded)
	cmd.Env = append(os.Environ(), "TASKVIEW_OWNER_PID="+strconv.Itoa(ownerPID))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 /* CREATE_NO_WINDOW */}

	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// utf16Encode encodes UTF-16LE WITHOUT a byte order mark.
//
// -EncodedCommand expects bare UTF-16LE. A leading BOM is decoded as U+FEFF,
// which PowerShell reads as part of the first token: the command then appears
// to be named "?$ErrorActionPreference" and fails with a CLIXML error.
// unicode/utf16.Encode is used so surrogate pairs above the BMP survive.
func utf16Encode(text []rune) []byte {
	units := utf16.Encode(text)
	out := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}

// powershellPath returns the full path to powershell.exe, so the helpers do not
// depend on PATH lookup.
func powershellPath() string {
	system := os.Getenv("SystemRoot")
	if system == "" {
		system = `C:\Windows`
	}
	return filepath.Join(system, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}
