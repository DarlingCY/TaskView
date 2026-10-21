//go:build windows

// Windows-specific installer tests: task identity, task definition, Windows
// command-line quoting, UTF-16 encoding and error classification.
//
// Real task registration is opt-in via TASKVIEW_TEST_SCHEDULER=1 and uses only
// unique temporary tasks with cleanup; default unit tests do not modify scheduled tasks.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

func TestScheduledTaskNameIsDeterministicAndIsolated(t *testing.T) {
	a := scheduledTaskName(`C:\Users\someone\.taskview`)
	if a != scheduledTaskName(`C:\Users\someone\.taskview`) {
		t.Error("same data dir produced different task names")
	}
	if !strings.HasPrefix(a, "TaskView-") {
		t.Errorf("task name = %q, want a TaskView- prefix", a)
	}
	// Different data dirs must not collide, so a TASKVIEW_HOME test run can never
	// touch the user's own default install.
	if scheduledTaskName(`C:\Users\someone\.taskview-alt`) == a {
		t.Error("different data dirs produced the same task name")
	}
	// Case-insensitive on Windows: the same directory spelled differently must
	// map to the same task rather than a duplicate.
	if scheduledTaskName(`C:\Users\Someone\.TaskView`) != a {
		t.Error("task name should be case-insensitive on Windows")
	}
}

func TestCurrentUserIdentityUsesAPISID(t *testing.T) {
	sid, name := currentUserIdentity()
	if sid == "" {
		t.Error("currentUserIdentity returned an empty SID")
	}
	if strings.HasPrefix(sid, "S-") == false {
		t.Errorf("SID = %q, want a Windows SID (S-1-5-...)", sid)
	}
	if name == "" {
		t.Error("currentUserIdentity returned an empty account name")
	}
}

func TestScheduledTaskCommandPinsStableBinaryAndDataDir(t *testing.T) {
	managed := `C:\Users\someone\.taskview\bin\taskview.exe`
	args := scheduledTaskCommand(managed, `C:\Users\someone\.taskview`)
	joined := strings.Join(args, " ")

	for _, want := range []string{"--background", "--data-dir"} {
		if !strings.Contains(joined, want) {
			t.Errorf("registered command %q missing %q", joined, want)
		}
	}
	// --data-dir must be absolute: Task Scheduler's working directory is
	// %SystemRoot%\System32 and would otherwise pick the wrong database.
	idx := indexOf(args, "--data-dir")
	if idx < 0 || idx+1 >= len(args) || !filepath.IsAbs(args[idx+1]) {
		t.Errorf("--data-dir must be absolute, got %v", args)
	}
}

// TestComposeTaskArgumentsQuotesOnlyArguments checks the helper the Exec action
// actually uses: arguments only, no argv[0], with correct Windows quoting.
func TestComposeTaskArgumentsQuotesOnlyArguments(t *testing.T) {
	// A data dir with a space must be quoted; no quoting is added otherwise.
	if got := composeTaskArguments([]string{"server", "run", "--data-dir", `C:\d e`}); got != `server run --data-dir "C:\d e"` {
		t.Errorf("arguments = %q, want the space-bearing value quoted", got)
	}
	if got := composeTaskArguments([]string{"server", "run"}); got != "server run" {
		t.Errorf("arguments = %q, want a plain form", got)
	}
	// An empty argument must be preserved as "" rather than dropped.
	if got := composeTaskArguments([]string{"server", "", "run"}); got != `server "" run` {
		t.Errorf("empty argument handling = %q, want it preserved", got)
	}
}

func TestTaskXMLIsValidAndUsesOnlyRealSettings(t *testing.T) {
	// Spaces, ampersand, apostrophe and non-ASCII all break naive handling.
	managed := `C:\Users\o'brien & 中文\bin\taskview.exe`
	dataDir := `C:\Users\o'brien & 中文\.taskview`

	doc, err := taskXML(managed, dataDir, scheduledTaskName(dataDir))
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}

	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"`,
		"<LogonTrigger>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<Command>", "<Arguments>", "<WorkingDirectory>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("task XML missing %q", want)
		}
	}

	// <StartWhenAvailableOnIdle> is not a TaskSettings property and made the
	// definition invalid.
	if strings.Contains(doc, "StartWhenAvailableOnIdle") {
		t.Error("task XML contains StartWhenAvailableOnIdle, which is not a valid TaskSettings element")
	}

	// InteractiveToken means no stored password.
	if strings.Contains(doc, "<Password>") {
		t.Error("task XML must not store a password")
	}

	// Encoding must match the bytes, which are UTF-8.
	if strings.Contains(doc, `encoding="UTF-16"`) {
		t.Error("declaration must say UTF-8 because the file is written as UTF-8")
	}
	if !utf8.ValidString(doc) {
		t.Error("task document must be valid UTF-8")
	}
	if !strings.Contains(doc, "中文") {
		t.Error("non-ASCII path characters should pass through as UTF-8")
	}

	// Escaped exactly once.
	if !strings.Contains(doc, "o&#39;brien &amp;") {
		t.Errorf("apostrophe/ampersand not escaped:\n%s", doc)
	}
	if strings.Contains(doc, "&amp;amp;") {
		t.Error("path was escaped twice")
	}

	// Command is a bare path, no shell quoting.
	cmdLine := betweenTags(doc, "<Command>", "</Command>")
	if strings.Contains(cmdLine, `"`) {
		t.Errorf("Command = %q, want a bare path", cmdLine)
	}
	if got := unescapeXMLText(cmdLine); got != managed {
		t.Errorf("Command = %q, want %q", got, managed)
	}

	// Well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(doc))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("task document is not well-formed XML: %v\n%s", err, doc)
			break
		}
	}
}

func TestUTF16EncodeHandlesSurrogatePairsWithoutBOM(t *testing.T) {
	// An emoji is above the BMP and must encode as a surrogate pair. A naive
	// per-rune byte loop truncates it to the low surrogate.
	emoji := "\U0001F600"
	units := utf16.Encode([]rune(emoji))
	if len(units) != 2 {
		t.Fatalf("emoji should encode as 2 UTF-16 units, got %d", len(units))
	}

	encoded := utf16Encode([]rune(emoji))
	// Two units, no BOM: -EncodedCommand rejects a BOM.
	if len(encoded) != 4 {
		t.Fatalf("encoded length = %d, want 4 (2 units, no BOM)", len(encoded))
	}
	if encoded[0] == 0xFF && encoded[1] == 0xFE {
		t.Error("utf16Encode emitted a BOM")
	}
	if uint16(encoded[0])|uint16(encoded[1])<<8 != units[0] ||
		uint16(encoded[2])|uint16(encoded[3])<<8 != units[1] {
		t.Error("encoded units do not match utf16.Encode")
	}
}

func TestIsBinaryInUseErrorUsesNumericCodes(t *testing.T) {
	// Message text is localised on this machine, so only the numeric codes can
	// be matched reliably.
	if !isBinaryInUseError(windows.ERROR_SHARING_VIOLATION) {
		t.Error("ERROR_SHARING_VIOLATION should be recognised")
	}
	if !isBinaryInUseError(windows.ERROR_ACCESS_DENIED) {
		t.Error("ERROR_ACCESS_DENIED should be recognised")
	}
	if isBinaryInUseError(windows.ERROR_FILE_NOT_FOUND) {
		t.Error("ERROR_FILE_NOT_FOUND is not an in-use condition")
	}
}

func TestRunComTaskIsBounded(t *testing.T) {
	if taskRunTimeout <= 0 || taskRunTimeout > 60*time.Second {
		t.Errorf("taskRunTimeout = %v, want a bounded value", taskRunTimeout)
	}
}

// TestValidateTaskDefinitionAcceptsGeneratedDocument covers what the validation
// seam actually guarantees: a well-formed Task document with a matching
// encoding declaration is accepted, and empty/malformed input is not.
//
// It deliberately does not assert element-name validity against the TaskSettings
// schema: that check would need either the installed XSD or a real
// registration, and claiming otherwise would make this a rubber stamp.
func TestValidateTaskDefinitionAcceptsGeneratedDocument(t *testing.T) {
	doc, err := taskXML(`C:\Users\o'brien & 中文\bin\taskview.exe`,
		`C:\Users\o'brien & 中文\.taskview`, "TaskView-abc123")
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}

	if err := platformValidateTaskDefinition(doc); err != nil {
		t.Fatalf("generated task definition must validate: %v", err)
	}

	if err := platformValidateTaskDefinition(""); err == nil {
		t.Error("validation accepted an empty document")
	}
	if err := platformValidateTaskDefinition("<Task><unclosed>"); err == nil {
		t.Error("validation accepted malformed XML")
	}
	if err := platformValidateTaskDefinition("<NotATask/>"); err == nil {
		t.Error("validation accepted a wrong root element")
	}
	// A declaration that disagrees with the UTF-8 bytes written must be caught.
	wrongEncoding := strings.Replace(doc, `encoding="UTF-8"`, `encoding="UTF-16"`, 1)
	if err := platformValidateTaskDefinition(wrongEncoding); err == nil {
		t.Error("validation accepted a document declaring UTF-16 while written as UTF-8")
	}
}

// TestValidateTaskDefinitionCreatesNoTask confirms validation is side-effect
// free: it must never register anything.
//
// The COM seam is stubbed so the assertion is hermetic - checking the real
// scheduler for the absence of a task would itself require a query.
func TestValidateTaskDefinitionCreatesNoTask(t *testing.T) {
	doc, err := taskXML(`C:\a b\taskview.exe`, `C:\a b\.taskview`, scheduledTaskName(`C:\a b\.taskview`))
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}

	orig := runTaskScheduler
	runTaskScheduler = func(mode, taskName, xmlDoc string) error {
		t.Errorf("validation invoked the scheduler (mode %q)", mode)
		return nil
	}
	t.Cleanup(func() { runTaskScheduler = orig })

	if err := validateTaskDefinition(doc); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
}

// TestValidateTaskDefinitionReportsInvalidDocument is covered by
// TestValidateTaskDefinitionAcceptsGeneratedDocument above.

// TestComScriptRemoveBranchCannotRegister guards a bug this caught: a remove
// path that fell through into the registration branch would have created a task
// while trying to delete one.
func TestComScriptRemoveBranchCannotRegister(t *testing.T) {
	if !strings.Contains(comScript, "$root.DeleteTask($taskName, 0)") {
		t.Error("remove branch should call DeleteTask")
	}

	// Every branch terminates explicitly, so no path can reach RegisterTask
	// unless $mode is create.
	for _, exit := range []string{"exit 0", "exit 1", "exit 2"} {
		if !strings.Contains(comScript, exit) {
			t.Errorf("comScript missing %q", exit)
		}
	}
	if strings.Count(comScript, "exit 0") < 2 {
		t.Error("both the remove and create branches must exit 0 explicitly")
	}
	// Absence is detected from the HRESULT GetTask throws, unwrapped from the
	// MethodInvocationException PowerShell wraps it in. Anything else rethrows.
	if !strings.Contains(comScript, "$HR_NOT_FOUND = -2147024894") {
		t.Error("remove branch should identify ERROR_FILE_NOT_FOUND numerically")
	}
	if !strings.Contains(comScript, "function Unwrap(") {
		t.Error("remove branch should unwrap InnerException before comparing HResult")
	}
	if !strings.Contains(comScript, "throw") {
		t.Error("remove branch must rethrow anything that is not a missing task")
	}
}

// taskSettingsProperties is the element set TaskSettings actually defines.
// A generator that adds anything else produces a document Windows rejects with
// an opaque error, so the test pins the document to this list.
var taskSettingsProperties = map[string]bool{
	"MultipleInstancesPolicy":         true,
	"DisallowStartIfOnBatteries":      true,
	"StopIfGoingOnBatteries":          true,
	"AllowHardTerminate":              true,
	"StartWhenAvailable":              true,
	"RunOnlyIfNetworkAvailable":       true,
	"IdleSettings":                    true,
	"AllowStartOnDemand":              true,
	"Enabled":                         true,
	"RunOnlyIfIdle":                   true,
	"IdleWaitTimeout":                 true,
	"NetworkProfileName":              true,
	"RunOnlyIfNetworkProfileRequired": true,
	"WakeToRun":                       true,
	"ExecutionTimeLimit":              true,
	"Priority":                        true,
	"DeleteExpiredTaskAfter":          true,
	"RestartOnFailure":                true,
	"RestartInterval":                 true,
	"StopOnIdleEnd":                   true,
	"RestartOnIdle":                   true,
	"Compatibility":                   true,
	"UseUnifiedSchedulingEngine":      true,
	"MaintenanceSettings":             true,
	"Volatile":                        true,
	"Hidden":                          true,
}

// TestGeneratedTaskXMLUsesOnlyKnownProperties walks the generated document with a
// real XML parser and fails if any Settings element is not a documented
// TaskSettings property. This is the guard that would have caught
// <StartWhenAvailableOnIdle>, which was never a real property.
func TestGeneratedTaskXMLUsesOnlyKnownProperties(t *testing.T) {
	doc, err := taskXML(`C:\Users\o'brien & 中文\bin\taskview.exe`,
		`C:\Users\o'brien & 中文\.taskview`, "TaskView-abc123")
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}

	dec := xml.NewDecoder(strings.NewReader(doc))
	inSettings := false
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("document is not well-formed: %v", err)
		}

		switch node := token.(type) {
		case xml.StartElement:
			switch node.Name.Local {
			case "Settings":
				inSettings = true
			default:
				if inSettings && !taskSettingsProperties[node.Name.Local] {
					t.Errorf("<%s> is not a documented TaskSettings property", node.Name.Local)
				}
			}
		case xml.EndElement:
			if node.Name.Local == "Settings" {
				inSettings = false
			}
		}
	}
}

// TestTaskSchemaAbsentElementGuard pins the specific regression: the invented
// element must never reappear in generated output.
func TestTaskSchemaAbsentElementGuard(t *testing.T) {
	doc, err := taskXML(`C:\a b\taskview.exe`, `C:\a b\.taskview`, "TaskView-abc123")
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}
	if strings.Contains(doc, "StartWhenAvailableOnIdle") {
		t.Error("generated task XML contains StartWhenAvailableOnIdle, which is not a TaskSettings property")
	}
	if strings.Contains(doc, "Encoding=UTF-16") {
		t.Error("generated task XML declares UTF-16 while the file is written as UTF-8")
	}
	if !strings.Contains(doc, "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>") {
		t.Error("ExecutionTimeLimit must be PT0S; the TaskSettings default of PT72H would kill the server")
	}
}

// TestRemoveAbsentTaskReturnsErrTaskAbsent exercises the real COM path: asking
// to delete a task that was never registered must be reported as absent, not as
// a failure. GetTask throws HRESULT 0x80070002 for a missing task, so this is
// the behaviour Uninstall depends on for idempotency.
func TestRemoveAbsentTaskReturnsErrTaskAbsent(t *testing.T) {
	requirePowerShell(t)

	// A name that cannot collide with anything, and is never registered.
	name := fmt.Sprintf("TaskView-test-%s", randomSuffix(t))
	t.Cleanup(func() { _ = platformRunTaskScheduler("remove", name, "") })

	err := platformRunTaskScheduler("remove", name, "")
	if !errors.Is(err, errTaskAbsent) {
		t.Fatalf("removing an absent task = %v, want errTaskAbsent (exit 1)", err)
	}
}

// TestEncodedCommandExecutesWithoutBOM proves the encoding contract against a
// real PowerShell: UTF-16LE with a BOM makes the command appear to be named
// "?$ErrorActionPreference" and fail with CLIXML noise on stdout.
func TestEncodedCommandExecutesWithoutBOM(t *testing.T) {
	requirePowerShell(t)

	// Non-ASCII in the script exercises surrogate-pair encoding too.
	const script = "$ErrorActionPreference='Stop';[Console]::Write('OK 🐎')"
	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(script)))

	if got := utf16Encode([]rune(script)); len(got) < 2 || got[0] == 0xFF && got[1] == 0xFE {
		t.Errorf("utf16Encode must not emit a BOM, got leading bytes %v", got[:2])
	}

	cmd := exec.Command(powershellPath(), "-NoProfile", "-NonInteractive",
		"-OutputFormat", "Text", "-EncodedCommand", encoded)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("encoded command failed: %v (stdout=%q stderr=%q)", err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "OK") {
		t.Errorf("stdout = %q, want it to contain OK", out)
	}
	// A BOM failure surfaces as serialized progress/stream objects.
	if strings.Contains(out, "CLIXML") || strings.Contains(out, "<S:") {
		t.Errorf("stdout contains CLIXML junk, suggesting an encoding problem: %q", out)
	}
}

// TestUTF16EncodeSurrogatePairs checks the encoding directly, independent of any
// PowerShell version.
func TestUTF16EncodeSurrogatePairs(t *testing.T) {
	// U+1F600 is above the BMP and must become two UTF-16 code units.
	units := utf16.Encode([]rune("😀"))
	if len(units) != 2 {
		t.Fatalf("emoji should encode as 2 units, got %d", len(units))
	}

	encoded := utf16Encode([]rune("😀"))
	if len(encoded) != 4 {
		t.Fatalf("encoded length = %d, want 4 (2 units, no BOM)", len(encoded))
	}
	if uint16(encoded[0])|uint16(encoded[1])<<8 != units[0] ||
		uint16(encoded[2])|uint16(encoded[3])<<8 != units[1] {
		t.Error("encoded units do not match utf16.Encode")
	}
	// No BOM.
	if encoded[0] == 0xFF && encoded[1] == 0xFE {
		t.Error("utf16Encode emitted a BOM; -EncodedCommand treats it as part of the command")
	}

	if got := utf16Encode([]rune("A")); len(got) != 2 || got[0] != 'A' {
		t.Errorf("ASCII encoding = %v, want 2 bytes without BOM", got)
	}
}

// TestTaskArgumentsExcludeExecutable guards the Exec action: the path belongs in
// <Command>, so repeating it in <Arguments> would make TaskView receive the
// executable path as its first command word.
func TestTaskArgumentsExcludeExecutable(t *testing.T) {
	managed := `C:\Users\someone\.taskview\bin\taskview.exe`
	dataDir := `C:\Users\someone\.taskview`

	args := scheduledTaskCommand(managed, dataDir)
	if len(args) == 0 || args[0] != "--background" {
		t.Fatalf("scheduledTaskCommand must start in background mode, got %v", args)
	}

	got := composeTaskArguments(args)
	if strings.Contains(got, managed) || strings.Contains(got, ".exe") {
		t.Errorf("arguments %q must not repeat the executable path", got)
	}
	if !strings.HasPrefix(got, "--background --data-dir") {
		t.Errorf("arguments %q should start in background mode with an explicit data directory", got)
	}

	// And the generated document must agree.
	doc, err := taskXML(managed, dataDir, "TaskView-abc123")
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}
	xmlArgs := unescapeXMLText(betweenTags(doc, "<Arguments>", "</Arguments>"))
	if strings.Contains(xmlArgs, ".exe") {
		t.Errorf("<Arguments> = %q, must not contain the executable path", xmlArgs)
	}
	if !strings.HasPrefix(xmlArgs, "--background --data-dir") {
		t.Errorf("<Arguments> = %q, want background mode with an explicit data directory", xmlArgs)
	}
	// <Command> still carries the path.
	cmdLine := unescapeXMLText(betweenTags(doc, "<Command>", "</Command>"))
	if !strings.Contains(cmdLine, "taskview.exe") {
		t.Errorf("<Command> = %q, want the executable path", cmdLine)
	}
}

func TestSelfDeleteScriptExecutesInPowerShell(t *testing.T) {
	requirePowerShell(t)

	dir := t.TempDir()
	dummyFile := filepath.Join(dir, "taskview_😀_test.exe")
	if err := os.WriteFile(dummyFile, []byte("dummy-binary-bytes"), 0o755); err != nil {
		t.Fatalf("write dummy file: %v", err)
	}

	sum, err := fileDigest(dummyFile)
	if err != nil {
		t.Fatalf("fileDigest: %v", err)
	}

	guard := selfDeleteGuard{
		Path:     dummyFile,
		SHA256:   sum,
		LockFile: filepath.Join(dir, "install.lock"),
		Manifest: filepath.Join(dir, "install.json"), // does not exist
	}
	script := selfDeleteScript(guard)

	// Use owner PID 0 which will immediately be not running in Get-Process
	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(script)))
	cmd := exec.Command(powershellPath(), "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
	cmd.Env = append(os.Environ(), "TASKVIEW_OWNER_PID=2147483640")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("powershell execution failed: %v, output: %s", err, string(out))
	}

	// Verify the file with emoji was successfully removed by PowerShell
	if _, err := os.Stat(dummyFile); !os.IsNotExist(err) {
		t.Fatalf("dummy file %s was not deleted by powershell; stat err: %v", dummyFile, err)
	}
}

func TestComScriptStripsXMLDeclaration(t *testing.T) {
	requirePowerShell(t)

	// Verify strip regex in actual PowerShell: removes XML declaration and preserves Unicode & XML content
	testXML := `<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>TaskView "quotes" &amp; 中文 😀</Description>
  </RegistrationInfo>
</Task>`

	psScript := `$xml = [System.IO.File]::ReadAllText($env:TEST_INPUT_XML, [System.Text.Encoding]::UTF8)
$stripped = $xml -replace '^\s*<\?xml[^?]*\?>\s*', ''
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::Out.Write($stripped)`

	tmpFile := filepath.Join(t.TempDir(), "test_decl.xml")
	if err := os.WriteFile(tmpFile, []byte(testXML), 0o600); err != nil {
		t.Fatalf("write temp xml: %v", err)
	}

	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(psScript)))
	cmd := exec.Command(powershellPath(), "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
	cmd.Env = append(os.Environ(), "TEST_INPUT_XML="+tmpFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("powershell strip execution failed: %v: %s", err, string(out))
	}

	gotStripped := string(out)
	if strings.HasPrefix(strings.TrimSpace(gotStripped), "<?xml") {
		t.Errorf("XML declaration was not stripped by PowerShell regex: %q", gotStripped)
	}
	if !strings.Contains(gotStripped, `<Task version="1.4"`) {
		t.Errorf("Task root element missing in stripped output: %q", gotStripped)
	}
	if !strings.Contains(gotStripped, "中文") || !strings.Contains(gotStripped, "😀") {
		t.Errorf("Unicode content (Chinese / emoji) damaged in stripped output: %q", gotStripped)
	}
	if !strings.Contains(gotStripped, `"quotes" &amp;`) {
		t.Errorf("XML entities damaged in stripped output: %q", gotStripped)
	}
}

// TestComScriptRealSchedulerRegistration exercises the real Windows Task Scheduler COM registration
// and deletion with a unique disposable task. It is skipped by default to avoid mutating the system
// scheduler in standard CI and requires explicit opt-in via TASKVIEW_TEST_SCHEDULER=1.
func TestComScriptRealSchedulerRegistration(t *testing.T) {
	if os.Getenv("TASKVIEW_TEST_SCHEDULER") != "1" {
		t.Skip("skipping real Task Scheduler registration test; opt-in with TASKVIEW_TEST_SCHEDULER=1")
	}
	requirePowerShell(t)

	taskName := fmt.Sprintf("TaskView-TestIsolated-%s", randomSuffix(t))
	dataDir := filepath.Join(t.TempDir(), "data")
	managed := filepath.Join(dataDir, "bin", "taskview.exe")

	doc, err := taskXML(managed, dataDir, taskName)
	if err != nil {
		t.Fatalf("taskXML: %v", err)
	}
	if !strings.Contains(doc, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("taskXML should declare UTF-8 in file: %s", doc)
	}

	t.Cleanup(func() {
		_ = platformRunTaskScheduler("remove", taskName, "")
	})

	// Create should succeed without COM encoding error (1,40)
	if err := platformRunTaskScheduler("create", taskName, doc); err != nil {
		t.Fatalf("platformRunTaskScheduler create failed: %v", err)
	}

	// Remove should succeed and delete the task cleanly
	if err := platformRunTaskScheduler("remove", taskName, ""); err != nil {
		t.Fatalf("platformRunTaskScheduler remove failed: %v", err)
	}
}

func requirePowerShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(powershellPath()); err != nil {
		t.Skipf("powershell.exe unavailable: %v", err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func betweenTags(doc, open, close string) string {
	start := strings.Index(doc, open)
	if start < 0 {
		return ""
	}
	rest := doc[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func unescapeXMLText(s string) string {
	out := s
	for _, pair := range [][2]string{
		{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&#34;", `"`}, {"&#39;", "'"},
	} {
		out = strings.ReplaceAll(out, pair[0], pair[1])
	}
	return out
}

func TestUninstallToleratesAbsentTask(t *testing.T) {
	_, _, runner := installEnv(t)

	runner.setFailure(errors.New("ERROR: The system cannot find the file specified."))
	if _, err := Install(InstallOptions{Autostart: true}); err == nil {
		t.Fatal("install should surface the registration failure")
	}
	// A never-registered task reports errTaskAbsent, which uninstall ignores.
	runner.setFailure(errTaskAbsent)

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall must tolerate a missing task: %v", err)
	}
}

// TestSelfDeleteScriptGuardHandlesEmojiApostropheAndAmpersand checks the
// path reaches the helper intact through PowerShell quoting and the digest.
func TestSelfDeleteScriptGuardHandlesEmojiApostropheAndAmpersand(t *testing.T) {
	managed := `C:\Users\😀 é & it's\taskview.exe`
	dir := t.TempDir()

	// A real file so fileDigest produces a genuine digest.
	realFile := filepath.Join(dir, "taskview.exe")
	writeExecutable(t, realFile, "replacement-bytes")
	sum, err := fileDigest(realFile)
	if err != nil {
		t.Fatalf("fileDigest: %v", err)
	}

	guard := selfDeleteGuard{
		Path:     managed,
		SHA256:   sum,
		LockFile: filepath.Join(dir, "install.lock"),
		Manifest: filepath.Join(dir, "install.json"),
	}
	script := selfDeleteScript(guard)

	// Emoji (a surrogate pair in UTF-16) and the apostrophe must survive; the
	// apostrophe has to be doubled to stay inside the quoted literal.
	if !strings.Contains(script, "😀") {
		t.Error("emoji in the managed path was lost")
	}
	if !strings.Contains(script, "it''s") {
		t.Error("apostrophe in the managed path was not doubled for PowerShell")
	}
	if !strings.Contains(script, "&") {
		t.Error("ampersand in the managed path was lost")
	}
	if !strings.Contains(script, sum) {
		t.Error("script should carry the real digest")
	}
	if !strings.Contains(script, "Remove-Item -LiteralPath $f") {
		t.Error("script should delete only the exact managed path")
	}
}

func TestQuoteForPowerShellEscapesQuotes(t *testing.T) {
	got := quoteForPowerShell(`C:\it's\here`)
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Fatalf("quoteForPowerShell = %q, want a quoted literal", got)
	}
	if strings.Count(got, "'") != 4 {
		t.Errorf("quoteForPowerShell = %q, want the embedded quote doubled", got)
	}
}

func TestSelfDeleteScriptIsBoundedAndGuarded(t *testing.T) {
	guard := selfDeleteGuard{
		Path:     `C:\data\bin\taskview.exe`,
		SHA256:   "abc123",
		LockFile: `C:\data\.taskview\install.lock`,
		Manifest: `C:\data\.taskview\install.json`,
	}
	script := selfDeleteScript(guard)

	// Must not poll a bare PID forever: every wait is deadline bounded.
	if strings.Count(script, "deadline") < 3 {
		t.Errorf("script should bound all wait loops with deadlines: %s", script)
	}
	if !strings.Contains(script, "AddSeconds(30)") || !strings.Contains(script, "AddSeconds(10)") {
		t.Errorf("script should use bounded waits: %s", script)
	}

	// Hash guard alone cannot spot a reinstall that wrote identical bytes, so
	// the manifest check is what distinguishes a fresh install.
	if !strings.Contains(script, "$manifest") {
		t.Error("script must check the install manifest: a reinstall writes identical bytes")
	}
	if !strings.Contains(script, `C:\data\.taskview\install.json`) {
		t.Error("script should reference the manifest path")
	}
	manifestGuard := strings.Index(script, "Test-Path -LiteralPath $manifest")
	digestGuard := strings.Index(script, "Get-FileHash")
	if manifestGuard < 0 || digestGuard < 0 || manifestGuard > digestGuard {
		t.Error("the manifest check must run before the delete attempt")
	}

	// The helper must take the same exclusive byte lock install/uninstall use.
	if !strings.Contains(script, "[System.IO.FileShare]::None") {
		t.Error("script should open install.lock with FileShare::None")
	}
	if !strings.Contains(script, "[System.IO.File]::Open($lock") {
		t.Error("script should open the install.lock byte range")
	}
	if !strings.Contains(script, `C:\data\.taskview\install.lock`) {
		t.Error("script should reference the lock path")
	}
	// Without the lock it must bail out rather than delete.
	lockGuard := strings.Index(script, "if($null -eq $lockHandle){exit 0}")
	if lockGuard < 0 || lockGuard > digestGuard {
		t.Error("script must bail out when the lock cannot be taken")
	}

	// Exact-path deletion only.
	if !strings.Contains(script, "Get-FileHash") || !strings.Contains(script, "abc123") {
		t.Error("script should verify the digest before deleting")
	}
	if !strings.Contains(script, "-LiteralPath") {
		t.Error("script should use -LiteralPath")
	}
	for _, forbidden := range []string{"-Recurse", "rm -rf", "rmdir", "del /", "Remove-Item -Recurse"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script contains %q, which could delete beyond the managed file", forbidden)
		}
	}
}

func TestUninstallClearsPendingMarkerWhenRemovalIsInline(t *testing.T) {
	installEnv(t)

	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	// The fake binary is not a real running image, so removal succeeds inline.
	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got := SelfDeleteScheduled(); got != "" {
		t.Errorf("SelfDeleteScheduled() = %q, want empty after an inline removal", got)
	}
	mustNotExist(t, result.Path)
}

func TestDeferredSelfDeleteIsScheduledForRunningManagedBinary(t *testing.T) {
	installEnv(t)

	result, err := Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Stand in for the running-binary case. A real image cannot be deleted while
	// executing, so scheduleSelfDelete must run and record the path.
	orig := executablePath
	executablePath = func() (string, error) { return result.Path, nil }
	t.Cleanup(func() { executablePath = orig })

	if !runningManagedBinary(result.Path) {
		t.Fatal("test setup failed: runningManagedBinary should report true")
	}

	scheduleErr := scheduleSelfDelete(result.Path)
	if scheduleErr != nil {
		t.Fatalf("scheduleSelfDelete: %v", scheduleErr)
	}

	// scheduleSelfDelete records the pending path before launching the helper,
	// and Uninstall must not clear it afterwards.
	selfDeletePending.mu.Lock()
	pending := selfDeletePending.path
	selfDeletePending.mu.Unlock()
	if pending != result.Path {
		t.Errorf("pending path = %q, want %q", pending, result.Path)
	}
}

// compile-time guards that os/exec and windows stay referenced.
var _ = exec.Command
var _ = os.Environ
var _ = filepath.Join
