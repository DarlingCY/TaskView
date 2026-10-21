//go:build windows

// Windows automatic start.
//
// The state is read from the Task Scheduler itself, through COM, so the answer
// is the one the scheduler would act on. The install manifest is never
// consulted: it records what the last install did, not what is in effect.
package main

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// platformAutostartRegistration reads this installation's registration from
// the Task Scheduler.
//
// Only the task that belongs to this data directory is inspected: its name is
// derived from the data directory, so a task registered by another
// installation - or by anything else entirely - is never read and never
// touched.
func platformAutostartRegistration(dataDir string) (autostartRegistration, error) {
	reg, err := queryScheduledTask(dataDir)
	if err != nil {
		return autostartRegistration{}, err
	}
	// The data directory is compared the way the task definition resolves it.
	reg.MatchesDataDir = matchesDataDir(reg.Args, absoluteDataDir(dataDir))
	return reg, nil
}

// platformAutostartEnabled reports whether the registration would really start
// exe with this data directory.
func platformAutostartEnabled(exe, dataDir string) (bool, error) {
	reg, err := platformAutostartRegistration(dataDir)
	if err != nil {
		return false, err
	}
	return reg.Enables(exe), nil
}

// platformAutostartApply enables or disables the automatic start.
//
// Enabling registers the logon task; disabling removes exactly that task and
// nothing else. The installed program, the data directory and every other
// setting stay untouched: switching the autostart off is not an uninstall.
//
// The caller has to hold the install lock, so this never takes it itself.
func platformAutostartApply(exe, dataDir string, enabled bool) error {
	if enabled {
		return platformRegisterAutostart(exe, dataDir)
	}
	return platformRemoveAutostart(dataDir)
}

// platformRestoreAutostart registers a previously captured task definition
// again.
//
// The definition is checked before it reaches the scheduler, so a broken
// snapshot fails here instead of as an opaque COM error. Registering replaces
// this installation's own task and nothing else.
//
// The caller has to hold the install lock, so this never takes it itself.
func platformRestoreAutostart(dataDir, definition string) error {
	trimmed := strings.TrimSpace(definition)
	if trimmed == "" {
		return errors.New("empty task definition")
	}
	// The captured document carries the scheduler's own encoding declaration.
	// Go's XML decoder rejects a declaration for anything but UTF-8, and the
	// registration path drops the declaration as well, so it is checked
	// without it.
	var doc struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(stripXMLDeclaration(trimmed)), &doc); err != nil {
		return fmt.Errorf("the captured task definition is not well-formed XML: %w", err)
	}
	if doc.XMLName.Local != "Task" {
		return fmt.Errorf("the captured task definition has a <%s> root, want <Task>", doc.XMLName.Local)
	}
	return runTaskScheduler("create", scheduledTaskName(dataDir), trimmed)
}

// stripXMLDeclaration removes the declaration of an XML document.
func stripXMLDeclaration(doc string) string {
	trimmed := strings.TrimSpace(doc)
	if !strings.HasPrefix(trimmed, "<?xml") {
		return trimmed
	}
	if idx := strings.Index(trimmed, "?>"); idx >= 0 {
		return strings.TrimSpace(trimmed[idx+2:])
	}
	return trimmed
}

// autostartQueryScript asks the Task Scheduler about this installation's logon
// task.
//
// The task name arrives on stdin. Nothing else is passed in: the paths and the
// command line are read back from the scheduler and compared here, in Go, as
// arguments and not as text.
//
// It writes one key=value line per fact on stdout:
//
//	exists=true|false   a task with this installation's name is registered
//	enabled=true|false   that task's own enabled flag
//	actions=<count>      how many actions the task has
//	path64=<base64>      the executable the task launches
//	args64=<base64>      the command line the task runs
//	xml64=<base64>       the task definition, for restoring the registration
//
// The paths are base64 encoded because a task definition is free to contain
// line breaks.
//
// A task that is not registered is not an error: the facts simply say so.
//
// The script only reads. It contains no registration and no deletion, so a
// query can never change the scheduler.
const autostartQueryScript = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$taskName = [Console]::In.ReadLine()

# HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND) == -2147024894 (0x80070002).
$HR_NOT_FOUND = -2147024894

# The COM error is wrapped in a MethodInvocationException, so every level of
# the chain is checked: the HRESULT sits on the innermost exception.
function IsNotFound($e) {
  $cur = $e
  while ($cur -is [System.Exception]) {
    if ($cur.HResult -eq $HR_NOT_FOUND) { return $true }
    $cur = $cur.InnerException
  }
  return $false
}

function B64([string]$s) {
  if ([string]::IsNullOrEmpty($s)) { return '' }
  return [System.Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($s))
}

function EmitAbsent {
  [Console]::Out.WriteLine('exists=false')
  [Console]::Out.WriteLine('enabled=false')
  [Console]::Out.WriteLine('actions=0')
  [Console]::Out.WriteLine('path64=')
  [Console]::Out.WriteLine('args64=')
  [Console]::Out.WriteLine('xml64=')
}

try {
  $svc = New-Object -ComObject 'Schedule.Service'
  $svc.Connect()
  $root = $svc.GetFolder('\')

  $task = $null
  try {
    $task = $root.GetTask($taskName)
  } catch {
    if (IsNotFound $_.Exception) {
      EmitAbsent
      exit 0
    }
    throw
  }
  if ($null -eq $task) {
    EmitAbsent
    exit 0
  }

  $enabled = $false
  try { $enabled = [bool]$task.Enabled } catch { $enabled = $false }

  $actions = 0
  $actionPath = ''
  $actionArgs = ''
  $def = $task.Definition
  if ($null -ne $def.Actions) {
    $actions = [int]$def.Actions.Count
    if ($actions -gt 0) {
      $action = $def.Actions.Item(1)
      $actionPath = [string]$action.Path
      $actionArgs = [string]$action.Arguments
    }
  }

  $xml = ''
  try { $xml = [string]$task.Xml } catch { $xml = '' }

  [Console]::Out.WriteLine('exists=true')
  if ($enabled) { [Console]::Out.WriteLine('enabled=true') } else { [Console]::Out.WriteLine('enabled=false') }
  [Console]::Out.WriteLine('actions=' + $actions)
  [Console]::Out.WriteLine('path64=' + (B64 $actionPath))
  [Console]::Out.WriteLine('args64=' + (B64 $actionArgs))
  [Console]::Out.WriteLine('xml64=' + (B64 $xml))
  exit 0
} catch {
  [Console]::Error.WriteLine($_.Exception.Message)
  exit 2
}
`

// queryScheduledTask asks the Task Scheduler about this installation's logon
// task and returns what it reports.
//
// The scheduler is reached through a short PowerShell script: this project has
// no COM binding for Go, and the scheduler's own messages are localised while
// an HRESULT is a number.
func queryScheduledTask(dataDir string) (autostartRegistration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskRunTimeout)
	defer cancel()

	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(autostartQueryScript)))
	// The command is encoded, so it needs no execution policy to be relaxed.
	cmd := exec.CommandContext(ctx, powershellPath(),
		"-NoProfile", "-NonInteractive", "-OutputFormat", "Text",
		"-EncodedCommand", encoded)
	cmd.Stdin = strings.NewReader(scheduledTaskName(dataDir) + "\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return autostartRegistration{}, fmt.Errorf("task scheduler query timed out after %s", taskRunTimeout)
		}
		return autostartRegistration{}, fmt.Errorf("task scheduler query failed: %w: %s",
			err, strings.TrimSpace(stderr.String()))
	}

	return parseAutostartFacts(stdout.String())
}

// parseAutostartFacts reads the key=value lines the query script writes.
//
// Unknown lines are ignored, so output the scheduler adds on its own cannot
// break the query.
func parseAutostartFacts(output string) (autostartRegistration, error) {
	var reg autostartRegistration

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "exists":
			reg.Exists = value == "true"
		case "enabled":
			reg.Enabled = value == "true"
		case "actions":
			count, err := strconv.Atoi(value)
			if err != nil {
				return autostartRegistration{}, fmt.Errorf("parse the task action count %q: %w", value, err)
			}
			reg.Actions = count
		case "path64":
			exe, err := decodeAutostartValue(value)
			if err != nil {
				return autostartRegistration{}, err
			}
			reg.Exe = exe
		case "args64":
			raw, err := decodeAutostartValue(value)
			if err != nil {
				return autostartRegistration{}, err
			}
			// The action's command line is split the way the program itself
			// would see it, so a data directory is compared as a whole
			// argument and not as a piece of text.
			argv, err := windows.DecomposeCommandLine(raw)
			if err != nil {
				return autostartRegistration{}, fmt.Errorf("split the task command line %q: %w", raw, err)
			}
			reg.Args = argv
		case "xml64":
			definition, err := decodeAutostartValue(value)
			if err != nil {
				return autostartRegistration{}, err
			}
			reg.XML = definition
		}
	}
	return reg, nil
}

// decodeAutostartValue decodes one base64 encoded fact.
func decodeAutostartValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("decode a task fact: %w", err)
	}
	return string(raw), nil
}

// absoluteDataDir resolves the data directory the way the task definition
// does, so the comparison with the registered arguments matches.
func absoluteDataDir(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return filepath.Clean(dataDir)
	}
	return filepath.Clean(abs)
}
