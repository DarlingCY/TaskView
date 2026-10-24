package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"taskview/internal/db"
)

func runCLI(args []string, dbPath string) (int, string, string) {
	fullArgs := append([]string{"--db", dbPath}, args...)
	var stdout, stderr bytes.Buffer
	code := run(fullArgs, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func parseJSONMap(t *testing.T, data string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &m); err != nil {
		t.Fatalf("failed to parse JSON map from %q: %v", data, err)
	}
	return m
}

func parseJSONArray(t *testing.T, data string) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &arr); err != nil {
		t.Fatalf("failed to parse JSON array from %q: %v", data, err)
	}
	return arr
}

func parseCLIError(t *testing.T, data string) CLIError {
	t.Helper()
	var errObj CLIError
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &errObj); err != nil {
		t.Fatalf("failed to parse CLIError from %q: %v", data, err)
	}
	return errObj
}

// 1. Help and Version do not touch the DB
func TestHelpAndVersionNoDB(t *testing.T) {
	nonExistentDir := filepath.Join(t.TempDir(), "nonexistent", "sub")
	dbPath := filepath.Join(nonExistentDir, "tasks.db")

	for _, flag := range []string{"--help", "-h", "help"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--db", dbPath, flag}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("help (%s) exited with %d", flag, code)
		}
		if !strings.Contains(stdout.String(), "taskview-cli") {
			t.Fatalf("help text missing from stdout: %s", stdout.String())
		}
	}

	for _, flag := range []string{"--version", "-v", "version"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--db", dbPath, flag}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("version (%s) exited with %d", flag, code)
		}
		if !strings.Contains(stdout.String(), "taskview-cli") {
			t.Fatalf("version text missing from stdout: %s", stdout.String())
		}
	}

	// Verify DB was NOT created
	if _, err := os.Stat(nonExistentDir); !os.IsNotExist(err) {
		t.Fatalf("help/version created directory %s", nonExistentDir)
	}
}

// 2. Health check
func TestCLIHealth(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "health.db")

	code, stdout, stderr := runCLI([]string{"health"}, dbPath)
	if code != 0 {
		t.Fatalf("health failed with code %d: stderr=%s", code, stderr)
	}
	res := parseJSONMap(t, stdout)
	if res["success"] != true {
		t.Fatalf("health response success != true: %v", res)
	}
}

// 3. Task Creation & Listing lifecycle
func TestCLITasksCreateAndList(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "tasks.db")
	projPath := filepath.Join(tmp, "测试 工程", "my-app")

	// Missing project-path rejected
	code, _, stderr := runCLI([]string{"tasks", "create", "--title", "Missing project"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected code %d on missing project-path, got %d: %s", ExitValidationError, code, stderr)
	}

	// Relative project-path rejected
	code, _, stderr = runCLI([]string{"tasks", "create", "--project-path", "relative/path", "--title", "Rel"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected code %d on relative project-path, got %d: %s", ExitValidationError, code, stderr)
	}

	// Missing title rejected
	code, _, stderr = runCLI([]string{"tasks", "create", "--project-path", projPath}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected code %d on missing title, got %d: %s", ExitValidationError, code, stderr)
	}

	// Valid task creation with flags
	code, stdout, stderr := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "测试任务 1",
		"--description", "详细中文说明",
		"--status", "todo",
		"--priority", "high",
		"--file-path", "src/main.go",
		"--line-start", "10",
		"--line-end", "20",
		"--code-snippet", "func main() {}",
		"--agent-notes", "initial note",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed with %d: stderr=%s", code, stderr)
	}
	task1 := parseJSONMap(t, stdout)
	id1, _ := task1["id"].(string)
	if id1 == "" {
		t.Fatalf("missing task id: %v", task1)
	}
	if task1["title"] != "测试任务 1" || task1["priority"] != "high" || task1["project_name"] != "my-app" {
		t.Fatalf("unexpected task fields: %v", task1)
	}

	// Empty list for different project
	diffProj := filepath.Join(tmp, "other-project")
	code, stdout, stderr = runCLI([]string{"tasks", "list", "--project-path", diffProj}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("list failed with %d: stderr=%s", code, stderr)
	}
	arr := parseJSONArray(t, stdout)
	if len(arr) != 0 {
		t.Fatalf("expected 0 tasks for other project, got %d", len(arr))
	}

	// List for projPath
	code, stdout, stderr = runCLI([]string{"tasks", "list", "--project-path", projPath}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("list failed with %d: stderr=%s", code, stderr)
	}
	arr = parseJSONArray(t, stdout)
	if len(arr) != 1 || arr[0]["id"] != id1 {
		t.Fatalf("expected task %s in list, got: %v", id1, arr)
	}
	// The CLI listing is the agent-facing surface: unlike GET /api/board/tasks
	// it must keep the full task body.
	if arr[0]["code_snippet"] != "func main() {}" {
		t.Errorf("tasks list lost code_snippet: %v", arr[0]["code_snippet"])
	}
	if arr[0]["agent_notes"] != "initial note" {
		t.Errorf("tasks list lost agent_notes: %v", arr[0]["agent_notes"])
	}
	if _, ok := arr[0]["has_agent_notes"]; ok {
		t.Errorf("tasks list carries the board-only has_agent_notes field: %v", arr[0])
	}

	// Filter by status and priority
	code, stdout, _ = runCLI([]string{"tasks", "list", "--project-path", projPath, "--status", "todo", "--priority", "high"}, dbPath)
	if code != ExitSuccess || len(parseJSONArray(t, stdout)) != 1 {
		t.Fatalf("expected 1 task on matching filter, got %s", stdout)
	}

	code, stdout, _ = runCLI([]string{"tasks", "list", "--project-path", projPath, "--status", "done"}, dbPath)
	if code != ExitSuccess || len(parseJSONArray(t, stdout)) != 0 {
		t.Fatalf("expected 0 tasks on non-matching status filter, got %s", stdout)
	}

	// Multiple status rejected
	code, _, stderr = runCLI([]string{"tasks", "list", "--project-path", projPath, "--status", "todo,in_progress"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected validation error on comma-separated status, got %d: %s", code, stderr)
	}
}

// 4. Strict JSON parsing and validation
func TestCLIStrictJSON(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "json.db")
	projPath := filepath.Join(tmp, "project")

	cases := []struct {
		name string
		json string
	}{
		{"empty string", ""},
		{"syntax error", `{"title": not-valid}`},
		{"trailing garbage", `{"title":"test","project_path":"/tmp"} trailing`},
		{"trailing object", `{"title":"test","project_path":"/tmp"} {"extra":1}`},
		{"JSON array", `[{"title":"array"}]`},
		{"JSON string", `"hello"`},
		{"JSON number", `1234`},
		{"JSON null", `null`},
		{"JSON bool", `true`},
		{"read-only id", fmt.Sprintf(`{"id":"T123","title":"test","project_path":%q}`, projPath)},
		{"read-only created_at", fmt.Sprintf(`{"created_at":123,"title":"test","project_path":%q}`, projPath)},
		{"read-only updated_at", fmt.Sprintf(`{"updated_at":123,"title":"test","project_path":%q}`, projPath)},
		{"read-only closed_at", fmt.Sprintf(`{"closed_at":123,"title":"test","project_path":%q}`, projPath)},
		{"read-only rejection_reason", fmt.Sprintf(`{"rejection_reason":"bad","title":"test","project_path":%q}`, projPath)},
		{"unknown field", fmt.Sprintf(`{"unknown_xyz":123,"title":"test","project_path":%q}`, projPath)},
		{"illegal status", fmt.Sprintf(`{"status":"invalid_status","title":"test","project_path":%q}`, projPath)},
		{"illegal priority", fmt.Sprintf(`{"priority":"super","title":"test","project_path":%q}`, projPath)},
		{"illegal line range", fmt.Sprintf(`{"line_start":50,"line_end":10,"title":"test","project_path":%q}`, projPath)},
		{"illegal line_start type", fmt.Sprintf(`{"line_start":"not-an-int","title":"test","project_path":%q}`, projPath)},
		{"line_start zero", fmt.Sprintf(`{"line_start":0,"title":"test","project_path":%q}`, projPath)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI([]string{"tasks", "create", "--json", tc.json}, dbPath)
			if code != ExitValidationError {
				t.Fatalf("%s: expected ExitValidationError (1), got %d: stderr=%s", tc.name, code, stderr)
			}
			errObj := parseCLIError(t, stderr)
			if errObj.Success != false || errObj.Code != "validation_error" {
				t.Fatalf("%s: invalid error JSON structure: %v", tc.name, errObj)
			}
		})
	}

	// Valid inline JSON creation
	validJSON := fmt.Sprintf(`{
		"project_path": %q,
		"title": "JSON Task",
		"description": "Created via --json",
		"status": "todo",
		"priority": "low",
		"line_start": 5,
		"line_end": 15
	}`, projPath)
	code, stdout, stderr := runCLI([]string{"tasks", "create", "--json", validJSON}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("valid JSON create failed with %d: stderr=%s", code, stderr)
	}
	task := parseJSONMap(t, stdout)
	if task["title"] != "JSON Task" {
		t.Fatalf("unexpected task title: %v", task)
	}
}

// 5. Tasks Get, Update, Null Clearing, and Project Verification
func TestCLITasksGetAndUpdate(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "update.db")
	projPath := filepath.Join(tmp, "project")

	// Create task
	code, stdout, stderr := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Task to Update",
		"--description", "initial desc",
		"--file-path", "file.txt",
		"--line-start", "10",
		"--line-end", "20",
		"--code-snippet", "snippet",
		"--agent-notes", "notes",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed: %s", stderr)
	}
	task := parseJSONMap(t, stdout)
	id := task["id"].(string)

	// Get task by ID
	code, stdout, stderr = runCLI([]string{"tasks", "get", id}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("get failed: %s", stderr)
	}
	got := parseJSONMap(t, stdout)
	if got["id"] != id || got["description"] != "initial desc" {
		t.Fatalf("get returned unexpected: %v", got)
	}

	// Get with matching project-path verification
	code, _, _ = runCLI([]string{"tasks", "get", id, "--project-path", projPath}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("get with matching project-path failed")
	}

	// Get with non-matching project-path verification
	code, _, stderr = runCLI([]string{"tasks", "get", id, "--project-path", filepath.Join(tmp, "other")}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected validation error on non-matching project-path, got %d: %s", code, stderr)
	}

	// Update with --json clearing nullable fields to null
	clearJSON := `{
		"description": null,
		"file_path": null,
		"line_start": null,
		"line_end": null,
		"code_snippet": null,
		"agent_notes": null,
		"status": "in_progress"
	}`
	code, stdout, stderr = runCLI([]string{"tasks", "update", id, "--json", clearJSON}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("update with null clearing failed: %s", stderr)
	}
	updated := parseJSONMap(t, stdout)
	if updated["status"] != "in_progress" {
		t.Fatalf("status not updated: %v", updated)
	}
	if updated["description"] != nil || updated["file_path"] != nil || updated["line_start"] != nil ||
		updated["line_end"] != nil || updated["code_snippet"] != nil || updated["agent_notes"] != nil {
		t.Fatalf("fields not cleared to null: %v", updated)
	}

	// Move to review (closed_at must be null)
	code, stdout, _ = runCLI([]string{"tasks", "update", id, "--status", "review"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("update to review failed")
	}
	updated = parseJSONMap(t, stdout)
	if updated["status"] != "review" || updated["closed_at"] != nil {
		t.Fatalf("status=review should have closed_at null: %v", updated)
	}

	// Move to done (closed_at must be set)
	code, stdout, _ = runCLI([]string{"tasks", "update", id, "--status", "done"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("update to done failed")
	}
	updated = parseJSONMap(t, stdout)
	if updated["status"] != "done" || updated["closed_at"] == nil {
		t.Fatalf("status=done should set closed_at: %v", updated)
	}

	// Reopen back to review (closed_at must be cleared)
	code, stdout, _ = runCLI([]string{"tasks", "update", id, "--status", "review"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("reopening to review failed")
	}
	updated = parseJSONMap(t, stdout)
	if updated["status"] != "review" || updated["closed_at"] != nil {
		t.Fatalf("reopening to review should clear closed_at: %v", updated)
	}
}

// 6. Tasks Notes and Tasks Reject (atomic operations)
func TestCLINotesAndReject(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "notes_reject.db")
	projPath := filepath.Join(tmp, "project")

	// Create task in review
	code, stdout, _ := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Review Task",
		"--status", "review",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed")
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	// Append note
	code, stdout, stderr := runCLI([]string{"tasks", "notes", id, "first note"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("append note 1 failed: %s", stderr)
	}
	updated := parseJSONMap(t, stdout)
	if updated["agent_notes"] != "first note" {
		t.Fatalf("agent_notes = %v, want 'first note'", updated["agent_notes"])
	}

	// Append second note
	code, stdout, stderr = runCLI([]string{"tasks", "notes", id, "--note", "second note"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("append note 2 failed: %s", stderr)
	}
	updated = parseJSONMap(t, stdout)
	if updated["agent_notes"] != "first note\nsecond note" {
		t.Fatalf("agent_notes = %v, want 'first note\nsecond note'", updated["agent_notes"])
	}

	// Empty note rejected
	code, _, stderr = runCLI([]string{"tasks", "notes", id, "   "}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected validation error on empty note, got %d: %s", code, stderr)
	}

	// Reject review -> in_progress
	code, stdout, stderr = runCLI([]string{"tasks", "reject", id, "Tests failed"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("reject failed: %s", stderr)
	}
	updated = parseJSONMap(t, stdout)
	if updated["status"] != "in_progress" || updated["rejection_reason"] != "Tests failed" {
		t.Fatalf("reject review failed: %v", updated)
	}

	// Reject in_progress -> backlog
	code, stdout, stderr = runCLI([]string{"tasks", "reject", id, "--reason", "Architecture change"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("reject in_progress failed: %s", stderr)
	}
	updated = parseJSONMap(t, stdout)
	if updated["status"] != "backlog" || updated["rejection_reason"] != "Architecture change" {
		t.Fatalf("reject in_progress failed: %v", updated)
	}

	// Reject backlog -> rejected with validation error
	code, _, stderr = runCLI([]string{"tasks", "reject", id, "cannot reject backlog"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected validation error on rejecting backlog, got %d: %s", code, stderr)
	}

	// Non-existent task notes/reject -> ExitNotFound (2)
	code, _, stderr = runCLI([]string{"tasks", "notes", "T999999999999", "note"}, dbPath)
	if code != ExitNotFound {
		t.Fatalf("expected ExitNotFound (2) for unknown task notes, got %d: %s", code, stderr)
	}
	code, _, stderr = runCLI([]string{"tasks", "reject", "T999999999999", "reason"}, dbPath)
	if code != ExitNotFound {
		t.Fatalf("expected ExitNotFound (2) for unknown task reject, got %d: %s", code, stderr)
	}
}

// 7. Concurrent Notes Append
func TestCLIConcurrentNotes(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "concurrency.db")
	projPath := filepath.Join(tmp, "project")

	code, stdout, _ := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Concurrent Task",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed")
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	const workerCount = 10
	var wg sync.WaitGroup
	wg.Add(workerCount)

	for i := 0; i < workerCount; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			c, _, errOut := runCLI([]string{"tasks", "notes", id, fmt.Sprintf("worker note %d", workerID)}, dbPath)
			if c != ExitSuccess {
				t.Errorf("worker %d note failed: %s", workerID, errOut)
			}
		}()
	}
	wg.Wait()

	code, stdout, _ = runCLI([]string{"tasks", "get", id}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("get failed")
	}
	task := parseJSONMap(t, stdout)
	notesStr, _ := task["agent_notes"].(string)
	lines := strings.Split(strings.TrimSpace(notesStr), "\n")
	if len(lines) != workerCount {
		t.Fatalf("expected %d notes lines, got %d: %s", workerCount, len(lines), notesStr)
	}
}

// 8. Shared Store with Board
func TestCLISharedWithBoard(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "shared.db")
	projPath := filepath.Join(tmp, "shared-proj")

	// CLI creates a task
	code, stdout, _ := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Shared CLI Task",
		"--status", "todo",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("cli create failed")
	}
	taskID := parseJSONMap(t, stdout)["id"].(string)

	// Board opens same store and verifies
	store, err := db.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("board OpenStore failed: %v", err)
	}
	defer store.Close()

	task, err := store.GetTask(taskID)
	if err != nil || task == nil {
		t.Fatalf("board could not read CLI created task: %v", err)
	}
	if task.Title != "Shared CLI Task" {
		t.Fatalf("board read mismatch: %v", task)
	}

	// Board updates task to review
	_, err = store.UpdateTask(taskID, map[string]any{"status": "review"})
	if err != nil {
		t.Fatalf("board update failed: %v", err)
	}

	// CLI reads task
	code, stdout, _ = runCLI([]string{"tasks", "get", taskID}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("cli get failed")
	}
	cliTask := parseJSONMap(t, stdout)
	if cliTask["status"] != "review" {
		t.Fatalf("cli did not see board update: %v", cliTask)
	}
}

// 9. Subprocess OS execution
func TestCLIProcessExecution(t *testing.T) {
	// The child build needs a native Go toolchain. Cross-compiled Linux test
	// binaries run on hosts without one; native CI still runs this test.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not in PATH; the native process smoke test is covered by the Linux/macOS CI jobs")
	}

	tmp := t.TempDir()
	binName := "taskview-cli"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	exePath := filepath.Join(tmp, binName)
	buildCmd := exec.Command("go", "build", "-o", exePath, ".")
	// The test binary may be started from any directory (for example a
	// cross-compiled binary run from the repository root). "go build ." has
	// to see this package, not the caller's working directory.
	buildCmd.Dir = cliPackageDir(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v: %s", err, string(out))
	}

	// Run health
	cmd := exec.Command(exePath, "health")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "TASKVIEW_HOME="+tmp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("process health failed: %v: %s", err, string(out))
	}
	if !strings.Contains(string(out), `"success":true`) {
		t.Fatalf("process health unexpected output: %s", string(out))
	}
}

// cliPackageDir returns the directory that holds the CLI sources, independent
// of the working directory of the (possibly cross-compiled) test binary.
func cliPackageDir(t *testing.T) string {
	t.Helper()
	// A binary built by "go test -c" and started from the repository (or from
	// its dist directory) finds the module root above itself.
	if exe, err := os.Executable(); err == nil {
		if dir := findCLIPackageFrom(filepath.Dir(exe)); dir != "" {
			return dir
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if dir := findCLIPackageFrom(wd); dir != "" {
			return dir
		}
	}
	// The native "go test" build usually keeps the sources visible through
	// the compiled file name.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the CLI test source")
	}
	return filepath.Dir(file)
}

func findCLIPackageFrom(start string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			candidate := filepath.Join(dir, "cmd", "taskview-cli")
			if _, err := os.Stat(filepath.Join(candidate, "main.go")); err == nil {
				return candidate
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// 9b. The argument and control-option boundaries through a real process.
//
// The in-process tests above cover every contract; this test makes sure the
// operating system hands the same boundaries to the built binary. In
// particular, "--" must stay data and a missing value must not be turned into
// a help or version request.
func TestCLIProcessRejectsMalformedArguments(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not in PATH; the native process smoke test is covered by the Linux/macOS CI jobs")
	}

	tmp := t.TempDir()
	binName := "taskview-cli"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	exePath := filepath.Join(tmp, binName)
	buildCmd := exec.Command("go", "build", "-o", exePath, ".")
	buildCmd.Dir = cliPackageDir(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v: %s", err, string(out))
	}

	home := filepath.Join(tmp, "home")
	dbPath := filepath.Join(home, "tasks.db")
	projPath := filepath.Join(tmp, "project")
	runProcess := func(args ...string) (int, string, string) {
		cmd := exec.Command(exePath, append([]string{"--db", dbPath}, args...)...)
		cmd.Env = append(os.Environ(), "TASKVIEW_HOME="+home)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			return ExitSuccess, stdout.String(), stderr.String()
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %v: %v", args, err)
		}
		return exitErr.ExitCode(), stdout.String(), stderr.String()
	}

	code, stdout, stderr := runProcess("tasks", "create", "--project-path", projPath, "--title", "process task")
	if code != ExitSuccess {
		t.Fatalf("create failed with %d: %s", code, stderr)
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	code, stdout, stderr = runProcess("tasks", "notes", id, "--", "--version")
	if code != ExitSuccess {
		t.Fatalf("notes with data argument failed with %d: %s", code, stderr)
	}
	if got := parseJSONMap(t, stdout)["agent_notes"]; got != "--version" {
		t.Fatalf("agent_notes = %v, want the literal --version", got)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing value followed by help", []string{"tasks", "update", id, "--title", "--help"}},
		{"unknown json field", []string{"tasks", "notes", id, "--json", `{"note":"n","typo":1}`}},
		{"two sources for one note", []string{"tasks", "notes", id, "--note", "flag", "--json", `{"note":"json"}`}},
		{"repeated json field in a process", []string{"tasks", "notes", id, "--json", `{"note":"one","note":"two"}`}},
		{"extra positional argument on health", []string{"health", "extra"}},
		{"help carrying a value", []string{"--help=yes"}},
		{"unknown field flag on update", []string{"tasks", "update", id, "--status", "todo", "--note", "ignored"}},
		{"extra positional argument on update", []string{"tasks", "update", id, "extra", "--title", "x"}},
		{"conflicting id", []string{"tasks", "get", id, "--id", "other"}},
		{"json with two spellings of one field", []string{"tasks", "update", id, "--json", `{"filePath":"camel.go","file_path":"snake.go"}`}},
		{"empty json next to a field flag", []string{"tasks", "update", id, "--json=", "--title", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runProcess(tc.args...)
			if code != ExitValidationError {
				t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, ExitValidationError, stdout, stderr)
			}
			if strings.Contains(stdout, "Usage:") || strings.Contains(stdout, "taskview-cli dev") {
				t.Fatalf("a rejected command was answered as a control option: %q", stdout)
			}
			if errObj := parseCLIError(t, stderr); errObj.Code != "validation_error" {
				t.Fatalf("stderr is not a validation error: %v", errObj)
			}
		})
	}

	code, stdout, stderr = runProcess("tasks", "get", id)
	if code != ExitSuccess {
		t.Fatalf("final get failed with %d: %s", code, stderr)
	}
	task := parseJSONMap(t, stdout)
	if task["title"] != "process task" || task["agent_notes"] != "--version" {
		t.Fatalf("a rejected command changed the task: %v", task)
	}
}

// 10. OS-aware path case sensitivity
func TestCLIPathCaseSensitivity(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "casesens.db")
	projPath := filepath.Join(tmp, "my_project")

	code, stdout, stderr := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Case Sensitivity Task",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed: %s", stderr)
	}
	task := parseJSONMap(t, stdout)
	id := task["id"].(string)

	upperProj := filepath.Join(tmp, "MY_PROJECT")
	code, stdout, _ = runCLI([]string{"tasks", "list", "--project-path", upperProj}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("list failed with %d", code)
	}
	arr := parseJSONArray(t, stdout)
	if runtime.GOOS == "windows" {
		if len(arr) != 1 {
			t.Fatalf("Windows must match case-insensitively, got %d tasks", len(arr))
		}
	} else {
		if len(arr) != 0 {
			t.Fatalf("Unix must match case-sensitively, got %d tasks", len(arr))
		}
	}

	// Verify project-path verification
	code, _, _ = runCLI([]string{"tasks", "get", id, "--project-path", upperProj}, dbPath)
	if runtime.GOOS == "windows" {
		if code != ExitSuccess {
			t.Fatalf("Windows get --project-path should match case-insensitively")
		}
	} else {
		if code != ExitValidationError {
			t.Fatalf("Unix get --project-path must be case-sensitive, got code %d", code)
		}
	}
}

// 11. Unknown flags, missing values, misplaced arguments and conflicting
//
// The CLI is the interface of an automated agent: a misspelled flag, a flag
// without its value or a value that arrives from two sources must be reported
// as an error and must not reach the database.
func TestCLIRejectsMalformedArguments(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "args.db")
	projPath := filepath.Join(tmp, "project")

	code, stdout, stderr := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Guarded Task",
		"--status", "todo",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed with %d: %s", code, stderr)
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	// The task as it looks before the rejected commands ran.
	before := parseJSONMap(t, runCLIGet(t, id, dbPath))

	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag on health", []string{"health", "--bogus", "abc"}},
		{"extra positional argument on health", []string{"health", "extra"}},
		{"positional argument on health after separator", []string{"health", "--", "extra"}},
		{"json with a repeated field", []string{"tasks", "update", id, "--json", `{"title":"one","title":"two"}`}},
		{"notes json with a repeated note", []string{"tasks", "notes", id, "--json", `{"note":"one","note":"two"}`}},
		{"reject json with a repeated reason", []string{"tasks", "reject", id, "--json", `{"reason":"one","reason":"two"}`}},
		{"unknown flag on list", []string{"tasks", "list", "--project-path", projPath, "--bogus"}},
		{"unknown field flag on update", []string{"tasks", "update", id, "--status", "todo", "--note", "silently ignored"}},
		{"extra positional argument on update", []string{"tasks", "update", id, "extra", "--title", "extra position accepted"}},
		{"flag without a value on update", []string{"tasks", "update", id, "--title"}},
		{"conflicting id on get", []string{"tasks", "get", id, "--id", "other"}},
		{"extra positional argument on get", []string{"tasks", "get", id, "extra"}},
		{"empty json next to a field flag", []string{"tasks", "update", id, "--json=", "--title", "x"}},
		{"conflicting json and field flag on create", []string{"tasks", "create", "--json", `{"title":"a","project_path":"` + projPath + `"}`, "--title", "b"}},
		{"conflicting json and project path on create", []string{"tasks", "create", "--json", `{"title":"a","project_path":"` + projPath + `"}`, "--project-path", projPath}},
		{"unknown json field on notes", []string{"tasks", "notes", id, "--json", `{"note":"real note","typo":"ignored"}`}},
		{"flag wins over json on notes", []string{"tasks", "notes", id, "--note", "flag wins", "--json", `{"note":"json lost"}`}},
		{"json and positional note", []string{"tasks", "notes", id, "positional", "--json", `{"note":"json"}`}},
		{"unknown json field on reject", []string{"tasks", "reject", id, "--json", `{"reason":"r","typo":1}`}},
		{"flag wins over json on reject", []string{"tasks", "reject", id, "--reason", "flag wins", "--json", `{"reason":"json lost"}`}},
		{"json and positional reason", []string{"tasks", "reject", id, "positional", "--json", `{"reason":"json"}`}},
		{"missing flag value on notes", []string{"tasks", "notes", id, "--note"}},
		{"missing flag value on reject", []string{"tasks", "reject", id, "--reason"}},
		{"empty note value", []string{"tasks", "notes", id, "--note="}},
		{"empty json value on notes", []string{"tasks", "notes", id, "--json="}},
		{"missing value hidden behind version", []string{"tasks", "notes", id, "--note", "--version"}},
		{"missing value hidden behind help", []string{"tasks", "reject", id, "--reason", "--help"}},
		{"duplicate flag on update", []string{"tasks", "update", id, "--title", "one", "--title", "two"}},
		{"empty status filter", []string{"tasks", "list", "--project-path", projPath, "--status="}},
		{"empty priority filter", []string{"tasks", "list", "--project-path", projPath, "--priority="}},
		{"empty limit filter", []string{"tasks", "list", "--project-path", projPath, "--limit="}},
		{"empty project check on get", []string{"tasks", "get", id, "--verify-project-path="}},
		{"empty project check on update", []string{"tasks", "update", id, "--verify-project-path="}},
		{"project check is not accepted by notes", []string{"tasks", "notes", id, "--verify-project-path", projPath, "note"}},
		{"project check is not accepted by reject", []string{"tasks", "reject", id, "--project-path", projPath, "reason"}},
		{"unknown flag on get", []string{"tasks", "get", id, "--bogus", "x"}},
		{"unknown flag on reject", []string{"tasks", "reject", id, "--bogus", "x"}},
		{"unknown flag on notes", []string{"tasks", "notes", id, "--bogus", "x"}},
		{"help with a value", []string{"--help=x"}},
		{"version with a value", []string{"--version=x"}},
		{"help with a value on a command", []string{"health", "--help=x"}},
		{"json with two spellings of one field", []string{"tasks", "update", id, "--json", `{"filePath":"camel.go","file_path":"snake.go"}`}},
		{"json with a misspelt field", []string{"tasks", "update", id, "--json", `{"filepath":"dropped.go"}`}},
		{"json with an upper-case field", []string{"tasks", "update", id, "--json", `{"TITLE":"wrong"}`}},
		{"create json with two spellings of one field", []string{"tasks", "create", "--json", `{"project_path":"` + projPath + `","projectPath":"` + projPath + `","title":"t"}`}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(tc.args, dbPath)
			if code != ExitValidationError {
				t.Fatalf("expected exit code %d, got %d (stdout=%q stderr=%q)", ExitValidationError, code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("a rejected command wrote to stdout: %q", stdout)
			}
			errObj := parseCLIError(t, stderr)
			if errObj.Success != false || errObj.Code != "validation_error" {
				t.Fatalf("invalid error JSON: %v", errObj)
			}
		})
	}

	// Not one of the rejected commands may have changed the task.
	after := parseJSONMap(t, runCLIGet(t, id, dbPath))
	if after["title"] != before["title"] || after["status"] != before["status"] ||
		after["agent_notes"] != before["agent_notes"] || after["project_path"] != before["project_path"] ||
		after["rejection_reason"] != before["rejection_reason"] {
		t.Fatalf("the task changed although every command failed:\nbefore %v\nafter  %v", before, after)
	}
}

// runCLIGet reads a task and fails the test when that is not possible.
func runCLIGet(t *testing.T, id, dbPath string) string {
	t.Helper()
	code, stdout, stderr := runCLI([]string{"tasks", "get", id}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("get %s failed with %d: %s", id, code, stderr)
	}
	return stdout
}

// 12. The control options are parsed, not guessed.
//
// "--help" and "--version" may only act as options. A value behind "--" is
// data, and a command with a broken flag is an error instead of a help
// screen.
func TestCLIControlOptionsAreParsed(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "control.db")
	projPath := filepath.Join(tmp, "project")

	code, stdout, stderr := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Control Task",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed with %d: %s", code, stderr)
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	// A missing value is not a help request.
	code, stdout, stderr = runCLI([]string{"tasks", "update", id, "--title", "--help"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a missing flag value, got %d (stdout=%q)", ExitValidationError, code, stdout)
	}
	if strings.Contains(stdout, "Usage:") {
		t.Fatalf("the missing flag value was turned into a help screen: %q", stdout)
	}

	// Everything behind "--" is data.
	code, stdout, stderr = runCLI([]string{"tasks", "notes", id, "--", "--version"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("notes with a data argument failed with %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "taskview-cli dev") && !strings.Contains(stdout, "agent_notes") {
		t.Fatalf("the data argument was answered as a version request: %q", stdout)
	}
	updated := parseJSONMap(t, stdout)
	if updated["agent_notes"] != "--version" {
		t.Fatalf("agent_notes = %v, want the literal text --version", updated["agent_notes"])
	}

	// The same rule applies to every value, not only the notes text.
	code, stdout, stderr = runCLI([]string{"tasks", "update", id, "--title", "--version"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a missing title value, got %d (stdout=%q)", ExitValidationError, code, stdout)
	}
	if strings.Contains(stdout, "taskview-cli") {
		t.Fatalf("the missing flag value was answered as a version request: %q", stdout)
	}

	// A help request is still a help request.
	code, stdout, stderr = runCLI([]string{"tasks", "update", id, "--title", "x", "--help"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("an explicit help request failed with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Usage:") {
		t.Fatalf("the help request did not print the usage: %q", stdout)
	}
}

// 13. The strict --json contract of notes and reject.
//
// Only the documented fields exist. An unknown field is a mistake that has to
// be reported and the value may not be taken from two sources at once.
func TestCLINotesAndRejectJSONContract(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "notes_json.db")
	projPath := filepath.Join(tmp, "project")

	code, stdout, _ := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "JSON Task",
		"--status", "review",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed with %d", code)
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	// The documented payload works.
	code, stdout, stderr := runCLI([]string{"tasks", "notes", id, "--json", `{"note":"from json"}`}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("notes with a json payload failed with %d: %s", code, stderr)
	}
	if parseJSONMap(t, stdout)["agent_notes"] != "from json" {
		t.Fatalf("the json payload was not appended: %s", stdout)
	}

	// The ID may come from the payload.
	code, stdout, stderr = runCLI([]string{"tasks", "notes", "--json", `{"id":"` + id + `","note":"payload id"}`}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("notes with an id in the payload failed with %d: %s", code, stderr)
	}
	if parseJSONMap(t, stdout)["agent_notes"] != "from json\npayload id" {
		t.Fatalf("the payload id was not used: %s", stdout)
	}

	// The same meaning may not come from the payload and from a flag.
	code, _, stderr = runCLI([]string{"tasks", "notes", id, "--note", "flag", "--json", `{"note":"json"}`}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a conflicting note, got %d: %s", ExitValidationError, code, stderr)
	}

	// The same meaning may not come from the payload and from the position.
	code, _, stderr = runCLI([]string{"tasks", "notes", id, "positional", "--json", `{"note":"json"}`}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a conflicting note, got %d: %s", ExitValidationError, code, stderr)
	}

	// The same meaning may not come from the payload and from --id.
	code, _, stderr = runCLI([]string{"tasks", "notes", "--id", id, "--json", `{"id":"` + id + `","note":"json"}`}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a conflicting id, got %d: %s", ExitValidationError, code, stderr)
	}

	// An unknown field is refused.
	code, _, stderr = runCLI([]string{"tasks", "notes", id, "--json", `{"note":"n","typo":1}`}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for an unknown field, got %d: %s", ExitValidationError, code, stderr)
	}

	// The reject command has the same contract.
	code, stdout, stderr = runCLI([]string{"tasks", "reject", id, "--json", `{"reason":"because"}`}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("reject with a json payload failed with %d: %s", code, stderr)
	}
	if parseJSONMap(t, stdout)["rejection_reason"] != "because" {
		t.Fatalf("the reason was not recorded: %s", stdout)
	}
	code, _, stderr = runCLI([]string{"tasks", "reject", id, "--json", `{"reason":"r","typo":1}`}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for an unknown field, got %d: %s", ExitValidationError, code, stderr)
	}
	code, _, stderr = runCLI([]string{"tasks", "reject", id, "--reason", "flag", "--json", `{"reason":"json"}`}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a conflicting reason, got %d: %s", ExitValidationError, code, stderr)
	}
}

// 14. Reading a task checks the project, changing a task moves it.
//
// The two meanings are different: --verify-project-path protects the task,
// --project-path on an update writes the new project.
func TestCLIProjectPathMeansDifferentThings(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "project_path.db")
	projPath := filepath.Join(tmp, "project")
	otherPath := filepath.Join(tmp, "other-project")

	code, stdout, stderr := runCLI([]string{
		"tasks", "create",
		"--project-path", projPath,
		"--title", "Scoped Task",
	}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("create failed with %d: %s", code, stderr)
	}
	id := parseJSONMap(t, stdout)["id"].(string)

	// The check of an update refuses a task of another project.
	code, _, stderr = runCLI([]string{"tasks", "update", id, "--verify-project-path", otherPath, "--status", "review"}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a foreign project, got %d: %s", ExitValidationError, code, stderr)
	}
	after := parseJSONMap(t, runCLIGet(t, id, dbPath))
	if after["status"] != "todo" {
		t.Fatalf("the refused update changed the status: %v", after)
	}

	// The check of a get refuses a task of another project as well.
	code, _, stderr = runCLI([]string{"tasks", "get", id, "--project-path", otherPath}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a foreign project, got %d: %s", ExitValidationError, code, stderr)
	}

	// The matching project is accepted.
	code, _, stderr = runCLI([]string{"tasks", "update", id, "--verify-project-path", projPath, "--status", "review"}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("the matching project was refused: %d %s", code, stderr)
	}

	// --project-path moves the task, which is a different act.
	code, stdout, stderr = runCLI([]string{"tasks", "update", id, "--project-path", otherPath}, dbPath)
	if code != ExitSuccess {
		t.Fatalf("the move failed with %d: %s", code, stderr)
	}
	if parseJSONMap(t, stdout)["project_path"] != otherPath {
		t.Fatalf("the task was not moved: %s", stdout)
	}

	// Giving both meanings of the check at once is refused.
	code, _, stderr = runCLI([]string{"tasks", "get", id, "--project-path", otherPath, "--verify-project-path", otherPath}, dbPath)
	if code != ExitValidationError {
		t.Fatalf("expected exit code %d for a duplicated project check, got %d: %s", ExitValidationError, code, stderr)
	}
}

// 15. The Skill and README describe the CLI that actually exists.
//
// The three documents are the interface an agent reads before it calls the
// CLI. A literal TAB in a Windows path or an archive name that the build
// never produces would send the agent to a nonexistent file, so the docs are
// checked like code: no TAB characters, the real release names, the real
// archive member name and the two meanings of the project path.
func TestDocumentationMatchesTheCLI(t *testing.T) {
	root := filepath.Dir(filepath.Dir(cliPackageDir(t)))
	docs := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "skills", "task-view", "SKILL.md"),
		filepath.Join(root, "skills", "task-view", "README.md"),
	}
	for _, doc := range docs {
		raw, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		text := string(raw)
		if strings.ContainsRune(text, '\t') {
			t.Errorf("%s contains a literal TAB; Windows paths must not be broken by indentation", doc)
		}

		// The published archive names carry the version and the extension;
		// a bare "taskview-cli-linux-amd64" does not exist.
		for _, bad := range []string{
			"taskview-cli-linux-amd64 ",
			"taskview-cli-linux-arm64 ",
			"taskview-cli-darwin-amd64 ",
			"taskview-cli-darwin-arm64 ",
			"taskview-cli-windows-amd64 ",
			"taskview-cli-windows-arm64 ",
		} {
			if strings.Contains(text, bad) {
				t.Errorf("%s names the unpublished artifact %q", doc, strings.TrimSpace(bad))
			}
		}
	}

	skill, err := os.ReadFile(docs[1])
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	skillText := string(skill)
	for _, want := range []string{
		"scripts/windows-amd64/taskview-cli.exe",
		"scripts/windows-arm64/taskview-cli.exe",
		"scripts/linux-amd64/taskview-cli",
		"scripts/linux-arm64/taskview-cli",
		"scripts/darwin-amd64/taskview-cli",
		"scripts/darwin-arm64/taskview-cli",
		"绝对路径",
		"WSL",
		"health` 会打开或初始化数据目录",
		"taskview-cli-<版本>-linux-amd64.tar.gz",
		"taskview-cli-<版本>-darwin-amd64.tar.gz",
		"taskview-cli-<版本>-windows-amd64.zip",
		"`taskview-cli`（不带版本号）",
		"update --verify-project-path",
		"update --project-path` 是**迁移任务到新工程**",
		"notes` / `reject` 没有工程保护参数",
		"PowerShell 7+",
		"cmd.exe",
	} {
		if !strings.Contains(skillText, want) {
			t.Errorf("SKILL.md does not contain %q", want)
		}
	}
}
