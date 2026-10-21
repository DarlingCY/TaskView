//go:build windows

// Windows autostart tests.
//
// Everything here is about the shape of the query: it must read the scheduler
// without ever changing it, and it must not paste user controlled data into
// the script it runs. No test in this file touches the real Task Scheduler.
package main

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutostartQueryScriptNeverChangesTheScheduler proves that the query is
// read-only: a question must never register or delete anything.
func TestAutostartQueryScriptNeverChangesTheScheduler(t *testing.T) {
	lower := strings.ToLower(autostartQueryScript)
	forbidden := []string{
		"registertask",
		"deletetask",
		"schtasks",
		"new-item",
		"remove-item",
		"set-scheduledtask",
		"enable-scheduledtask",
		"disable-scheduledtask",
		"unregister",
	}
	for _, word := range forbidden {
		if strings.Contains(lower, word) {
			t.Errorf("the query script must not contain %q", word)
		}
	}
}

// TestAutostartQueryScriptTakesUserDataFromTheEnvironment proves that the
// task name never ends up in the script text, where a quote or a semicolon in
// a path would change the meaning of the command.
//
// The data directory is not passed to the script at all any more: the command
// line is read back and compared here, as arguments.
func TestAutostartQueryScriptTakesUserDataFromTheEnvironment(t *testing.T) {
	if !strings.Contains(autostartQueryScript, "[Console]::In.ReadLine()") {
		t.Error("the task name must be read from stdin")
	}
	if strings.Contains(autostartQueryScript, "TASKVIEW_QUERY_DATA_DIR") {
		t.Error("the data directory must not be pasted into the script")
	}
	// A hard coded path in the script would be a path that bypasses the
	// environment entirely.
	if strings.Contains(autostartQueryScript, `C:\`) {
		t.Error("the query script must not contain a hard coded path")
	}
}

// TestAutostartQueryScriptEncodesWithoutBOM proves the encoding contract: a
// leading byte order mark would make PowerShell read the command as a name
// and fail.
func TestAutostartQueryScriptEncodesWithoutBOM(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(utf16Encode([]rune(autostartQueryScript)))
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		t.Error("the encoded script must not start with a byte order mark")
	}
}

// TestParseAutostartFactsReadsTheQueryOutput proves that the facts the script
// reports are read back correctly, including the command line, which is split
// the way the program itself would see it.
func TestParseAutostartFactsReadsTheQueryOutput(t *testing.T) {
	const doc = `<?xml version="1.0" encoding="UTF-16"?><Task><Settings><Enabled>true</Enabled></Settings></Task>`
	output := strings.Join([]string{
		"exists=true",
		"enabled=true",
		"actions=1",
		"path64=" + base64.StdEncoding.EncodeToString([]byte(`C:\app\taskview.exe`)),
		`args64=` + base64.StdEncoding.EncodeToString([]byte(`--background --data-dir "C:\data dir"`)),
		"xml64=" + base64.StdEncoding.EncodeToString([]byte(doc)),
		"",
	}, "\r\n")

	reg, err := parseAutostartFacts(output)
	if err != nil {
		t.Fatalf("parseAutostartFacts: %v", err)
	}
	if !reg.Exists {
		t.Error("the task should be reported as existing")
	}
	if !reg.Enabled {
		t.Error("the task should be reported as enabled")
	}
	if reg.Actions != 1 {
		t.Errorf("actions = %d, want 1", reg.Actions)
	}
	if reg.Exe != `C:\app\taskview.exe` {
		t.Errorf("exe = %q, want the registered executable", reg.Exe)
	}
	want := []string{"--background", "--data-dir", `C:\data dir`}
	if len(reg.Args) != len(want) {
		t.Fatalf("args = %q, want %q", reg.Args, want)
	}
	for i, arg := range want {
		if reg.Args[i] != arg {
			t.Errorf("args = %q, want %q", reg.Args, want)
			break
		}
	}
	if reg.XML != doc {
		t.Errorf("xml = %q, want the registered definition", reg.XML)
	}
}

// TestParseAutostartFactsIgnoresUnknownOutput proves that output the
// scheduler adds on its own cannot break the query.
func TestParseAutostartFactsIgnoresUnknownOutput(t *testing.T) {
	output := "some noise\r\nexists=false\r\nenabled=false\r\nactions=0\r\npath64=\r\nargs64=\r\nxml64=\r\n"

	reg, err := parseAutostartFacts(output)
	if err != nil {
		t.Fatalf("parseAutostartFacts: %v", err)
	}
	if reg.Exists {
		t.Error("the task should be reported as absent")
	}
	if reg.Exe != "" {
		t.Errorf("exe = %q, want no executable", reg.Exe)
	}
	if reg.XML != "" {
		t.Errorf("xml = %q, want no definition", reg.XML)
	}
}

// TestRestoreAutostartRegistersTheCapturedTask proves that a captured
// definition is registered again under this installation's own task name.
//
// The scheduler call is replaced, so no task is created.
func TestRestoreAutostartRegistersTheCapturedTask(t *testing.T) {
	orig := runTaskScheduler
	t.Cleanup(func() { runTaskScheduler = orig })

	var gotMode, gotName, gotXML string
	runTaskScheduler = func(mode, taskName, xmlDoc string) error {
		gotMode, gotName, gotXML = mode, taskName, xmlDoc
		return nil
	}

	const doc = `<?xml version="1.0" encoding="UTF-16"?><Task><Settings><Enabled>false</Enabled></Settings></Task>`
	if err := restoreAutostart(`C:\data`, doc); err != nil {
		t.Fatalf("restoreAutostart: %v", err)
	}
	if gotMode != "create" {
		t.Errorf("mode = %q, want create", gotMode)
	}
	if want := scheduledTaskName(`C:\data`); gotName != want {
		t.Errorf("task name = %q, want %q", gotName, want)
	}
	if gotXML != doc {
		t.Errorf("definition = %q, want the captured document", gotXML)
	}
}

// TestRestoreAutostartRejectsBrokenDefinitions proves that a broken snapshot
// fails before the scheduler is touched.
func TestRestoreAutostartRejectsBrokenDefinitions(t *testing.T) {
	orig := runTaskScheduler
	t.Cleanup(func() { runTaskScheduler = orig })
	runTaskScheduler = func(mode, taskName, xmlDoc string) error {
		t.Error("a broken definition must not reach the scheduler")
		return nil
	}

	cases := []string{
		"",
		"not xml at all",
		"<NotATask/>",
	}
	for _, doc := range cases {
		if err := restoreAutostart(`C:\data`, doc); err == nil {
			t.Errorf("restoreAutostart(%q) must fail", doc)
		}
	}
}

// TestAbsoluteDataDirMatchesTheTaskDefinition proves that the data directory
// is resolved the same way the task definition resolves it, so the comparison
// with the registered arguments matches.
func TestAbsoluteDataDirMatchesTheTaskDefinition(t *testing.T) {
	dir := t.TempDir()
	got := absoluteDataDir(dir)
	if !filepath.IsAbs(got) {
		t.Errorf("absoluteDataDir(%q) = %q, want an absolute path", dir, got)
	}
	if filepath.Clean(got) != filepath.Clean(dir) {
		t.Errorf("absoluteDataDir(%q) = %q, want %q", dir, got, filepath.Clean(dir))
	}
}
