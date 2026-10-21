package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "test_tasks.db"))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

// projectFlags returns the required --project / --project-name pair.
func projectFlags(path, name string) []string {
	return []string{"--project", path, "--project-name", name}
}

// newProject returns a fresh temp project dir and a name derived from it.
func newProject(t *testing.T, name string) (string, string) {
	t.Helper()
	return filepath.Join(t.TempDir(), name), name
}

func TestCLIHelpAndVersion(t *testing.T) {
	store := newTestStore(t)

	cases := [][]string{
		{},
		{"help"},
		{"--help"},
		{"-h"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Errorf("args %v expected code 0, got %d", args, code)
		}
		if !strings.Contains(stdout.String(), "TaskView") || !strings.Contains(stdout.String(), "USAGE") {
			t.Errorf("args %v expected help output, got: %s", args, stdout.String())
		}
	}

	versionCases := [][]string{
		{"version"},
		{"--version"},
		{"-v"},
	}
	for _, args := range versionCases {
		var stdout, stderr bytes.Buffer
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Errorf("args %v expected code 0, got %d", args, code)
		}
		if !strings.Contains(stdout.String(), "taskview 1.0.0") {
			t.Errorf("args %v expected version output, got: %s", args, stdout.String())
		}
	}
}

func TestCLIArgParser(t *testing.T) {
	store := newTestStore(t)

	t.Run("flags after positionals and --key=value", func(t *testing.T) {
		proj, projName := newProject(t, "alpha")
		var stdout, stderr bytes.Buffer
		args := append([]string{"add", "My Feature", "--priority=high", "--desc", "feature description"}, projectFlags(proj, projName)...)
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Task created") {
			t.Fatalf("expected task created, got: %s", stdout.String())
		}

		tasks, err := store.ListTasks(ListOptions{All: true})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("expected 1 task in store, got %d, err: %v", len(tasks), err)
		}
		if tasks[0].Title != "My Feature" || tasks[0].Priority != "high" || tasks[0].Description == nil || *tasks[0].Description != "feature description" {
			t.Fatalf("task fields mismatch: %+v", tasks[0])
		}
	})

	t.Run("dash-dash delimiter", func(t *testing.T) {
		proj, projName := newProject(t, "alpha")
		var stdout, stderr bytes.Buffer
		args := append([]string{"add"}, projectFlags(proj, projName)...)
		args = append(args, "--", "--starts-with-dashes")
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		tasks, err := store.ListTasks(ListOptions{All: true, Search: "starts-with-dashes"})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("expected 1 task with dashes, got %d", len(tasks))
		}
		if tasks[0].Title != "--starts-with-dashes" {
			t.Fatalf("expected title '--starts-with-dashes', got %q", tasks[0].Title)
		}
	})

	t.Run("unknown option rejection", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"add", "Invalid Flag Task", "--bogus-option", "123"}, store, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("expected nonzero exit code for unknown option, got 0")
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected 'unknown option' in stderr, got: %s", stderr.String())
		}
	})

	t.Run("missing option value rejection", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"add", "Missing Val Task", "--priority"}, store, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("expected nonzero exit code for missing flag value, got 0")
		}
		if !strings.Contains(stderr.String(), "requires an argument") {
			t.Fatalf("expected 'requires an argument' in stderr, got: %s", stderr.String())
		}
	})

	t.Run("invalid boolean flag rejected", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"list", "--all=banana"}, store, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("expected nonzero exit code for invalid boolean, got 0")
		}
		if !strings.Contains(stderr.String(), "invalid boolean value") {
			t.Fatalf("expected 'invalid boolean value' in stderr, got: %s", stderr.String())
		}
	})
}

func TestCLICRUDAndDispatch(t *testing.T) {
	store := newTestStore(t)
	proj, projName := newProject(t, "alpha")

	// 1. Add without title should fail
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"add"}, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for add without title, got %d", code)
		}
		if !strings.Contains(stderr.String(), "task title is required") {
			t.Fatalf("expected 'task title is required' in stderr, got: %s", stderr.String())
		}
	}

	// 2. Add with full options
	var taskID string
	{
		var stdout, stderr bytes.Buffer
		args := append([]string{
			"add", "Implement CLI tests",
			"--priority", "high",
			"--status", "in_progress",
			"--desc", "Unit and integration testing for CLI",
			"--file", "cli_test.go",
			"--lines", "10-50",
			"--notes", "initial notes",
		}, projectFlags(proj, projName)...)
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to add task: %s", stderr.String())
		}
		outStr := stdout.String()
		if !strings.Contains(outStr, "Task created") {
			t.Fatalf("expected 'Task created' in output, got: %s", outStr)
		}

		// Extract task ID from store
		tasks, err := store.ListTasks(ListOptions{All: true})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("expected 1 task in store, got %d", len(tasks))
		}
		taskID = tasks[0].ID
	}

	// 3. Get task
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"get", taskID}, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to get task: %s", stderr.String())
		}
		outStr := stdout.String()
		if !strings.Contains(outStr, "Implement CLI tests") || !strings.Contains(outStr, taskID) {
			t.Fatalf("get output missing task info: %s", outStr)
		}
	}

	// 4. Get missing id
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"get"}, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for get without id, got %d", code)
		}
	}

	// 5. Get unknown id
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"get", "task_nonexistent_123"}, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for get unknown id, got %d", code)
		}
		if !strings.Contains(stderr.String(), "task not found") {
			t.Fatalf("expected 'task not found' in stderr, got: %s", stderr.String())
		}
	}

	// 6. Update task fields and line numbers
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{
			"update", taskID,
			"--status", "done",
			"--lines", "100-200",
			"--append-notes", "completed all tests",
		}, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to update task: %s", stderr.String())
		}
		task, err := store.GetTask(taskID)
		if err != nil || task == nil {
			t.Fatalf("failed to retrieve task after update: %v", err)
		}
		if task.Status != "done" {
			t.Fatalf("expected status done, got %s", task.Status)
		}
		if task.LineStart == nil || *task.LineStart != 100 || task.LineEnd == nil || *task.LineEnd != 200 {
			t.Fatalf("expected lines 100-200, got: %v-%v", task.LineStart, task.LineEnd)
		}
		if task.AgentNotes == nil || !strings.Contains(*task.AgentNotes, "completed all tests") {
			t.Fatalf("expected appended notes, got %v", task.AgentNotes)
		}
	}

	// 6b. Optional clears via empty flags
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{
			"update", taskID,
			"--desc=",
			"--notes=",
			"--lines=",
		}, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to clear fields: %s", stderr.String())
		}
		task, err := store.GetTask(taskID)
		if err != nil || task == nil {
			t.Fatalf("failed to retrieve task after clear: %v", err)
		}
		if task.Description != nil || task.AgentNotes != nil || task.LineStart != nil || task.LineEnd != nil {
			t.Fatalf("expected cleared nullable fields to be nil, got: %+v", task)
		}
	}

	// 7. Update without fields
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"update", taskID}, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for empty update, got %d", code)
		}
		if !strings.Contains(stderr.String(), "nothing to update") {
			t.Fatalf("expected 'nothing to update' in stderr, got: %s", stderr.String())
		}
	}

	// 8. List tasks
	{
		var stdout, stderr bytes.Buffer
		args := append([]string{"list", "--all"}, projectFlags(proj, projName)...)
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to list tasks: %s", stderr.String())
		}
		outStr := stdout.String()
		if !strings.Contains(outStr, "Implement CLI tests") || !strings.Contains(outStr, "1 task") {
			t.Fatalf("unexpected list output: %s", outStr)
		}
	}

	// 9. Stats
	{
		// Path-scoped stats: --project is required and scopes the totals.
		{
			var stdout, stderr bytes.Buffer
			args := append([]string{"stats"}, projectFlags(proj, projName)...)
			code := RunCLI(args, store, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("failed to get project stats: %s", stderr.String())
			}
			if !strings.Contains(stdout.String(), "Task statistics") || strings.Contains(stdout.String(), "- all projects") {
				t.Fatalf("project stats should scope to the project, got: %s", stdout.String())
			}
		}

		// Global stats with --all (flags are still required).
		var stdout, stderr bytes.Buffer
		args := append([]string{"stats", "--all"}, projectFlags(proj, projName)...)
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to get stats: %s", stderr.String())
		}
		outStr := stdout.String()
		if !strings.Contains(outStr, "Task statistics - all projects") || !strings.Contains(outStr, "Done        1") {
			t.Fatalf("unexpected stats output: %s", outStr)
		}
	}

	// 10. Delete task
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"delete", taskID}, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to delete task: %s", stderr.String())
		}
		if !strings.Contains(stdout.String(), "Deleted") {
			t.Fatalf("expected 'Deleted' in stdout, got: %s", stdout.String())
		}

		// Verify deletion
		task, err := store.GetTask(taskID)
		if err != nil || task != nil {
			t.Fatalf("expected task to be deleted, got %v", task)
		}
	}

	// 11. Delete nonexistent task
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"delete", taskID}, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 when deleting nonexistent task, got %d", code)
		}
	}

	// 12. Unknown command
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"unknowncmd"}, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown command, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown command") {
			t.Fatalf("expected 'unknown command' in stderr, got: %s", stderr.String())
		}
	}
}

func TestCLIProjectsEmpty(t *testing.T) {
	store := newTestStore(t)

	var stdout, stderr bytes.Buffer
	code := RunCLI([]string{"projects"}, store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "No projects found.\n" {
		t.Fatalf("unexpected empty output: %q", got)
	}
}

func TestCLIProjectsListsAllProjects(t *testing.T) {
	store := newTestStore(t)
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")

	add := func(title, project, name, status string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args := append([]string{"add", title, "--status", status}, projectFlags(project, name)...)
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("add %q failed: %s", title, stderr.String())
		}
	}
	add("Alpha one", alpha, "alpha", "done")
	add("Alpha two", alpha, "alpha", "todo")
	add("Beta one", beta, "beta", "backlog")

	var stdout, stderr bytes.Buffer
	code := RunCLI([]string{"projects"}, store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("projects failed: %s", stderr.String())
	}
	out := stdout.String()

	for _, want := range []string{"PROJECT", "TASKS", "OPEN", "UPDATED", "PATH", alpha, beta} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2 projects - 3 tasks, 2 open") {
		t.Errorf("output missing summary line:\n%s", out)
	}

	// Alpha has 2 tasks / 1 open; beta has 1 task / 1 open, so alpha sorts first.
	lines := strings.Split(out, "\n")
	var alphaLine, betaLine int
	for i, line := range lines {
		if strings.Contains(line, alpha) {
			alphaLine = i
		}
		if strings.Contains(line, beta) {
			betaLine = i
		}
	}
	if alphaLine == 0 || betaLine == 0 || alphaLine > betaLine {
		t.Fatalf("expected alpha row before beta row, got:\n%s", out)
	}

	for _, row := range []struct {
		line              int
		name, total, open string
	}{
		{alphaLine, "alpha", "2", "1"},
		{betaLine, "beta", "1", "1"},
	} {
		fields := strings.Fields(lines[row.line])
		if len(fields) < 5 || fields[0] != row.name || fields[1] != row.total || fields[2] != row.open {
			t.Errorf("unexpected counts for %s: %q", row.name, lines[row.line])
		}
	}
}

func TestCLIProjectsGroupsIdenticalNamesByPath(t *testing.T) {
	store := newTestStore(t)
	root := t.TempDir()
	first := filepath.Join(root, "first", "app")
	second := filepath.Join(root, "second", "app")

	for _, project := range []string{first, second} {
		var stdout, stderr bytes.Buffer
		if code := RunCLI(append([]string{"add", "Shared name task"}, projectFlags(project, "app")...), store, &stdout, &stderr); code != 0 {
			t.Fatalf("add for %s failed: %s", project, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"projects"}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("projects failed: %s", stderr.String())
	}
	out := stdout.String()

	if strings.Count(out, "app ") < 2 || !strings.Contains(out, first) || !strings.Contains(out, second) {
		t.Fatalf("expected two separate app projects keyed by path, got:\n%s", out)
	}
	if !strings.Contains(out, "2 projects - 2 tasks, 2 open") {
		t.Fatalf("expected two projects, got:\n%s", out)
	}
}

func TestRenderProjectsDeterministicFormatting(t *testing.T) {
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()
	projects := []Project{
		{Path: "/work/alpha", Name: "alpha", TotalTasks: 12, OpenTasks: 5, UpdatedAt: old},
		{Path: "/work/beta", Name: "a-much-longer-name", TotalTasks: 3, OpenTasks: 0, UpdatedAt: 0},
	}

	var buf bytes.Buffer
	renderProjects(&buf, projects, ColorConfig{})
	out := buf.String()

	want := "" +
		"PROJECT             TASKS  OPEN  UPDATED     PATH\n" +
		"alpha               12     5     2020-01-02  /work/alpha\n" +
		"a-much-longer-name  3      0     -           /work/beta\n" +
		"-------------------------------------------------\n" +
		"2 projects - 15 tasks, 5 open\n"
	if out != want {
		t.Fatalf("unexpected projects rendering:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestRenderProjectsAlignsLargeCounts(t *testing.T) {
	var buf bytes.Buffer
	renderProjects(&buf, []Project{
		{Path: "/work/large", Name: "large", TotalTasks: 100000, OpenTasks: 10000},
		{Path: "/work/small", Name: "small", TotalTasks: 1, OpenTasks: 0},
	}, ColorConfig{})

	lines := strings.Split(buf.String(), "\n")
	for _, col := range []struct {
		header, large, small string
	}{
		{"TASKS", "100000", "1"},
		{"OPEN", "10000", "0"},
		{"UPDATED", "-", "-"},
		{"PATH", "/work/large", "/work/small"},
	} {
		index := strings.Index(lines[0], col.header)
		if index < 0 || strings.LastIndex(lines[1], col.large) != index || strings.LastIndex(lines[2], col.small) != index {
			t.Errorf("misaligned %s column:\n%s", col.header, buf.String())
		}
	}
}

func TestCLIProjectsRejectsArguments(t *testing.T) {
	store := newTestStore(t)

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"projects", "unexpected"}, store, &stdout, &stderr); code != 1 {
		t.Fatalf("expected code 1 for unexpected argument, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Fatalf("expected 'unexpected argument' in stderr, got: %s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunCLI([]string{"projects", "--bogus", "value"}, store, &stdout, &stderr); code != 1 {
		t.Fatalf("expected code 1 for unknown option, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown option") {
		t.Fatalf("expected 'unknown option' in stderr, got: %s", stderr.String())
	}
}

func TestCLIProjectScopeFlagsRequired(t *testing.T) {
	store := newTestStore(t)
	proj, projName := newProject(t, "alpha")

	// command-specific args that would otherwise succeed.
	cmds := map[string][]string{
		"add":   {"add", "Scoped task"},
		"list":  {"list"},
		"stats": {"stats"},
	}

	// The project flag is required for every command; only list/stats place the
	// name in the usage line, while add derives it from the path.
	projectCases := []struct {
		name string
		flag []string // appended to the command args
	}{
		{"missing project", []string{"--project-name", projName}},
		{"blank project", []string{"--project=", "--project-name", projName}},
		{"whitespace project", []string{"--project=   ", "--project-name", projName}},
		{"valueless project", []string{"--project=true", "--project-name", projName}},
	}
	nameCases := []struct {
		name string
		flag []string // appended to the command args
	}{
		{"missing name", []string{"--project", proj}},
		{"blank name", []string{"--project", proj, "--project-name="}},
		{"whitespace name", []string{"--project", proj, "--project-name=  "}},
		{"valueless name", []string{"--project", proj, "--project-name=true"}},
	}

	checkError := func(t *testing.T, args []string, want string, wantNameInUsage bool) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := RunCLI(args, store, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected exit code 1, got %d, stderr: %s", code, stderr.String())
		}
		errOut := stderr.String()
		if !strings.Contains(errOut, "Error:") || !strings.Contains(errOut, want) {
			t.Fatalf("expected Error containing %q, got: %s", want, errOut)
		}
		if !strings.Contains(errOut, "Usage:") || !strings.Contains(errOut, "--project <path>") {
			t.Fatalf("expected Usage line naming --project, got: %s", errOut)
		}
		if hasName := strings.Contains(errOut, "--project-name <name>"); hasName != wantNameInUsage {
			t.Fatalf("expected --project-name in usage=%v, got: %s", wantNameInUsage, errOut)
		}
	}

	for cmdName, base := range cmds {
		for _, tc := range projectCases {
			t.Run(cmdName+"/"+tc.name, func(t *testing.T) {
				args := append(append([]string{}, base...), tc.flag...)
				checkError(t, args, "--project is required", cmdName != "add")
			})
		}
	}
	for _, cmdName := range []string{"list", "stats"} {
		for _, tc := range nameCases {
			t.Run(cmdName+"/"+tc.name, func(t *testing.T) {
				args := append(append([]string{}, cmds[cmdName]...), tc.flag...)
				checkError(t, args, "--project-name is required", true)
			})
		}
	}

	// --all must not exempt the required flags.
	for _, args := range [][]string{{"list", "--all"}, {"stats", "--all"}} {
		var stdout, stderr bytes.Buffer
		if code := RunCLI(args, store, &stdout, &stderr); code != 1 {
			t.Fatalf("%v expected exit 1 without project flags, got %d", args, code)
		}
		if !strings.Contains(stderr.String(), "--project is required") {
			t.Fatalf("%v expected project error, got: %s", args, stderr.String())
		}
	}

	// Nothing should have been written while validating.
	if tasks, err := store.ListTasks(ListOptions{All: true}); err != nil || len(tasks) != 0 {
		t.Fatalf("expected no tasks, got %d (err=%v)", len(tasks), err)
	}
}

func TestCLIProjectScopeAliases(t *testing.T) {
	store := newTestStore(t)
	proj, projName := newProject(t, "aliased")

	var stdout, stderr bytes.Buffer
	code := RunCLI([]string{"add", "Aliased task", "--projectPath", proj, "--projectName", projName}, store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add with camelCase aliases failed: %s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunCLI([]string{"stats", "-C", proj, "--projectName", projName}, store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("stats with -C/--projectName failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Total       1") {
		t.Fatalf("expected stats for the aliased project, got: %s", stdout.String())
	}
}

func TestCLIProjectNameAdoptedAndEnforced(t *testing.T) {
	store := newTestStore(t)
	proj, _ := newProject(t, "ignored")

	// Seed a task recorded under chosen-name, as older versions did when add
	// accepted --project-name.
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"add", "First on path", "--project", proj}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("first add failed: %s", stderr.String())
	}
	db, err := store.conn()
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET project_name = 'chosen-name'`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(tasks), err)
	}
	if tasks[0].ProjectName != "chosen-name" {
		t.Fatalf("expected recorded name chosen-name, got %q", tasks[0].ProjectName)
	}

	// add on the same path reuses the recorded name, even with a different
	// --project-name supplied.
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI([]string{"add", "Second on path", "--project", proj, "--project-name", "other-name"}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("second add failed: %s", stderr.String())
	}

	// A different name for the same path is rejected for list and stats.
	for _, args := range [][]string{
		{"list", "--project", proj, "--project-name", "other-name"},
		{"stats", "--project", proj, "--project-name", "other-name"},
		{"stats", "--all", "--project", proj, "--project-name", "other-name"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := RunCLI(args, store, &stdout, &stderr); code != 1 {
			t.Fatalf("%v expected exit 1, got %d, stdout: %s", args, code, stdout.String())
		}
		errOut := stderr.String()
		if !strings.Contains(errOut, `project name "other-name" does not match recorded name "chosen-name"`) ||
			!strings.Contains(errOut, proj) {
			t.Fatalf("%v expected name mismatch error, got: %s", args, errOut)
		}
	}

	if tasks, err := store.ListTasks(ListOptions{All: true}); err != nil || len(tasks) != 2 {
		t.Fatalf("expected 2 tasks after rejections, got %d (err=%v)", len(tasks), err)
	}
	for _, task := range tasks {
		if task.ProjectName != "chosen-name" {
			t.Fatalf("expected both tasks under chosen-name, got %q", task.ProjectName)
		}
	}
}

func TestCLIAddDerivesProjectName(t *testing.T) {
	store := newTestStore(t)

	// (a) No --project-name: the directory name is recorded.
	proj, projName := newProject(t, "derived")
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"add", "No name", "--project", proj}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("add without --project-name failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "project="+projName) {
		t.Fatalf("expected project=%s in output, got: %s", projName, stdout.String())
	}

	// (b) A stray --project-name is ignored.
	other, otherName := newProject(t, "stray")
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI([]string{"add", "Stray name", "--project", other, "--project-name", "ignored-name"}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("add with stray --project-name failed: %s", stderr.String())
	}

	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	got := map[string]string{}
	for _, task := range tasks {
		got[task.ProjectPath] = task.ProjectName
	}
	if len(tasks) != 2 || got[proj] != projName || got[other] != otherName {
		t.Fatalf("expected directory-derived names, got tasks=%d names=%v", len(tasks), got)
	}
}

func TestCLIAddReusesRecordedName(t *testing.T) {
	store := newTestStore(t)
	proj, _ := newProject(t, "dir-name")

	// Seed a task recorded under a name different from the directory name.
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"add", "Seeded", "--project", proj}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("seed add failed: %s", stderr.String())
	}
	seeded, err := store.ListTasks(ListOptions{All: true})
	if err != nil || len(seeded) != 1 {
		t.Fatalf("expected 1 seeded task, got %d (err=%v)", len(seeded), err)
	}
	// Rename the recorded name behind the add path's back (add no longer
	// accepts an arbitrary name on a path that already has tasks).
	db, err := store.conn()
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET project_name = 'legacy'`); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Add without a name reuses the recorded name instead of the directory name.
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI([]string{"add", "Reused", "--project", proj}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("add on recorded path failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "project=legacy") {
		t.Fatalf("expected project=legacy in output, got: %s", stdout.String())
	}

	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d (err=%v)", len(tasks), err)
	}
	for _, task := range tasks {
		if task.ProjectName != "legacy" {
			t.Fatalf("expected all tasks under legacy, got %q", task.ProjectName)
		}
	}

	// Still a single project group.
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI([]string{"projects"}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("projects failed: %s", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "1 project - 2 tasks, 2 open") {
		t.Fatalf("expected one project group, got:\n%s", out)
	}
	// Exactly one data row (the path echoes "dir-name", which may also be the
	// auto-generated temp dir name).
	rows := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "legacy") {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("expected exactly one project row, got %d:\n%s", rows, out)
	}

	// list still accepts the recorded name and rejects the directory name.
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI(append([]string{"list"}, projectFlags(proj, "legacy")...), store, &stdout, &stderr); code != 0 {
		t.Fatalf("list with recorded name failed: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI(append([]string{"stats"}, projectFlags(proj, "dir-name")...), store, &stdout, &stderr); code != 1 {
		t.Fatalf("stats with directory name expected exit 1, got %d", code)
	}
}

func TestCLIListAndStatsRemainPathScoped(t *testing.T) {
	store := newTestStore(t)
	alpha, alphaName := newProject(t, "alpha")
	beta, betaName := newProject(t, "beta")

	for _, p := range []struct{ path, name, title string }{
		{alpha, alphaName, "Alpha task"},
		{beta, betaName, "Beta task"},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"add", p.title}, projectFlags(p.path, p.name)...)
		if code := RunCLI(args, store, &stdout, &stderr); code != 0 {
			t.Fatalf("add %q failed: %s", p.title, stderr.String())
		}
	}

	// list is scoped to the given path even without --all.
	var stdout, stderr bytes.Buffer
	if code := RunCLI(append([]string{"list"}, projectFlags(alpha, alphaName)...), store, &stdout, &stderr); code != 0 {
		t.Fatalf("list failed: %s", stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "Alpha task") || strings.Contains(out, "Beta task") {
		t.Fatalf("list not path-scoped: %s", out)
	}

	// stats totals stay path-scoped.
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI(append([]string{"stats"}, projectFlags(beta, betaName)...), store, &stdout, &stderr); code != 0 {
		t.Fatalf("stats failed: %s", stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "Total       1") || strings.Contains(out, "- all projects") {
		t.Fatalf("stats not path-scoped: %s", out)
	}

	// --all still lists every project.
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI(append([]string{"list", "--all"}, projectFlags(alpha, alphaName)...), store, &stdout, &stderr); code != 0 {
		t.Fatalf("list --all failed: %s", stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "Alpha task") || !strings.Contains(out, "Beta task") {
		t.Fatalf("list --all should show all projects: %s", out)
	}
}

func TestCLIHelpMentionsProjects(t *testing.T) {
	store := newTestStore(t)

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"help"}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"projects",
		"global and read-only",
		"Projects appear only after their first task is recorded.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestCLIHelpMentionsBackgroundServer(t *testing.T) {
	store := newTestStore(t)

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"help"}, store, &stdout, &stderr); code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"BACKGROUND SERVER",
		"make sure",
		"the background server is running",
		"starting it automatically when needed",
		"ui and server commands",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestCLIUIInvalidPort(t *testing.T) {
	store := newTestStore(t)

	cases := []string{"not-a-port", "99999", "-5", "true"}
	for _, p := range cases {
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"ui", "--port", p}, store, &stdout, &stderr)
		if code != 1 {
			t.Errorf("port %s expected exit code 1, got %d", p, code)
		}
		if !strings.Contains(stderr.String(), "invalid port") {
			t.Errorf("port %s expected 'invalid port' in stderr, got: %s", p, stderr.String())
		}
	}
}

func TestCLINoColorEnv(t *testing.T) {
	orig, had := os.LookupEnv("NO_COLOR")
	defer func() {
		if had {
			_ = os.Setenv("NO_COLOR", orig)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	}()

	// Even empty NO_COLOR presence must disable ANSI color
	_ = os.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	c := initColors(&buf)
	if c.Bold != "" || c.Reset != "" || c.Red != "" {
		t.Fatalf("expected empty colors when NO_COLOR is set, got: %+v", c)
	}
}

func TestUnicodeTruncation(t *testing.T) {
	var buf bytes.Buffer
	tasks := []Task{
		{
			ID:          "task_unicode_1234",
			Title:       "Testing UTF-8 truncation with emojis and CJK",
			Status:      "todo",
			Priority:    "medium",
			FilePath:    new(string),
			CreatedAt:   1000,
			UpdatedAt:   1000,
			ProjectName: "Project",
		},
	}
	*tasks[0].FilePath = "src/very/long/path/with/unicode/日本語_and_✨_sparkles.go"
	renderList(&buf, tasks, false, ColorConfig{})
	output := buf.String()
	if !strings.Contains(output, "…") {
		t.Fatalf("expected truncated location with '…', got: %s", output)
	}
}

func resetLifecycleHooks(t *testing.T) {
	t.Helper()
	origEnsure := lifecycleEnsureBackgroundServer
	origStatus := lifecycleManagedServerStatus
	origStop := lifecycleStopBackgroundServer
	origRun := lifecycleRunManagedServer
	origInstall := lifecycleInstall
	origUninstall := lifecycleUninstall
	origBrowser := lifecycleOpenBrowser

	t.Cleanup(func() {
		lifecycleEnsureBackgroundServer = origEnsure
		lifecycleManagedServerStatus = origStatus
		lifecycleStopBackgroundServer = origStop
		lifecycleRunManagedServer = origRun
		lifecycleInstall = origInstall
		lifecycleUninstall = origUninstall
		lifecycleOpenBrowser = origBrowser
	})
}

func TestCLIInstallDispatch(t *testing.T) {
	resetLifecycleHooks(t)

	var lastOpts InstallOptions
	var installCalled int
	lifecycleInstall = func(opts InstallOptions) (*InstallResult, error) {
		installCalled++
		lastOpts = opts
		return &InstallResult{
			Path:      filepath.Join("C:", "mock", "data", "bin", "taskview.exe"),
			Autostart: opts.Autostart,
		}, nil
	}

	// 1. Plain install
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"install"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if installCalled != 1 || lastOpts.Autostart {
			t.Fatalf("expected 1 call with Autostart=false, got %d, %+v", installCalled, lastOpts)
		}
		if !strings.Contains(stdout.String(), "Installed TaskView to") {
			t.Fatalf("expected installed message, got: %s", stdout.String())
		}
	}

	// 2. Install with --autostart
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"install", "--autostart"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if installCalled != 2 || !lastOpts.Autostart {
			t.Fatalf("expected Autostart=true, got %+v", lastOpts)
		}
		if !strings.Contains(stdout.String(), "Autostart on login enabled") {
			t.Fatalf("expected autostart enabled message, got: %s", stdout.String())
		}
	}

	// 3. Install with --autostart=false
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"install", "--autostart=false"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if installCalled != 3 || lastOpts.Autostart {
			t.Fatalf("expected Autostart=false, got %+v", lastOpts)
		}
	}

	// 4. Invalid boolean for --autostart
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"install", "--autostart=maybe"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for invalid boolean, got %d", code)
		}
		if !strings.Contains(stderr.String(), "invalid boolean value") {
			t.Fatalf("expected invalid boolean error, got: %s", stderr.String())
		}
	}

	// 5. Extra positional arg rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"install", "extra"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for extra arg, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unexpected argument") {
			t.Fatalf("expected unexpected argument error, got: %s", stderr.String())
		}
	}

	// 6. Unknown option rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"install", "--port", "3333"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown option, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option error, got: %s", stderr.String())
		}
	}
}

func TestCLIUninstallDispatch(t *testing.T) {
	resetLifecycleHooks(t)

	var uninstallCalled int
	lifecycleUninstall = func() error {
		uninstallCalled++
		return nil
	}

	// 1. Successful uninstall
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"uninstall"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if uninstallCalled != 1 {
			t.Fatalf("expected 1 uninstall call, got %d", uninstallCalled)
		}
		out := stdout.String()
		if !strings.Contains(out, "uninstalled successfully") || !strings.Contains(out, "Data retained at") {
			t.Fatalf("expected uninstall and data retained message, got: %s", out)
		}
	}

	// 2. Extra positional arg rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"uninstall", "now"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for extra arg, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unexpected argument") {
			t.Fatalf("expected unexpected argument error, got: %s", stderr.String())
		}
	}

	// 3. Unknown option rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"uninstall", "--autostart"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown option, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option error, got: %s", stderr.String())
		}
	}

	// 4. Uninstall with deferred binary deletion (SelfDeleteScheduled non-empty)
	{
		selfDeletePending.mu.Lock()
		selfDeletePending.path = filepath.Join("C:", "mock", "data", "bin", "taskview.exe")
		selfDeletePending.mu.Unlock()
		defer func() {
			selfDeletePending.mu.Lock()
			selfDeletePending.path = ""
			selfDeletePending.mu.Unlock()
		}()

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"uninstall"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "scheduled on process exit") || !strings.Contains(out, "Data retained at") {
			t.Fatalf("expected deferred deletion and data retained in stdout, got: %s", out)
		}
	}
}

func TestCLIServerSubcommandsDispatch(t *testing.T) {
	resetLifecycleHooks(t)

	var ensurePort int
	lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
		ensurePort = port
		return &ServerState{
			PID:       9988,
			URL:       "http://127.0.0.1:3333",
			DB:        "/test/tasks.db",
			Token:     "SECRET_TOKEN_DO_NOT_LEAK",
			Instance:  "inst123",
			StartedAt: 1700000000000,
		}, nil
	}

	var stopCalled int
	lifecycleStopBackgroundServer = func() error {
		stopCalled++
		return nil
	}

	var statusState *ServerState
	var statusErr error
	lifecycleManagedServerStatus = func() (*ServerState, error) {
		return statusState, statusErr
	}

	var runPort int
	lifecycleRunManagedServer = func(port int) error {
		runPort = port
		return nil
	}

	// 1. server start with default port
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if ensurePort != 3333 {
			t.Fatalf("expected default port 3333, got %d", ensurePort)
		}
		out := stdout.String()
		if !strings.Contains(out, "http://127.0.0.1:3333") || !strings.Contains(out, "PID 9988") || !strings.Contains(out, "Log path:") {
			t.Fatalf("expected URL, PID, and Log path in output, got: %s", out)
		}
		if strings.Contains(out, "SECRET_TOKEN") {
			t.Fatalf("token must NEVER be printed in stdout, got: %s", out)
		}
	}

	// 2. server start with explicit port and port 0 smoke test
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start", "--port", "0"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0 for port 0, got %d, stderr: %s", code, stderr.String())
		}
		if ensurePort != 0 {
			t.Fatalf("expected port 0, got %d", ensurePort)
		}
	}

	// 3. server start with invalid port
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start", "--port", "invalid-port"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for invalid port, got %d", code)
		}
		if !strings.Contains(stderr.String(), "invalid port") {
			t.Fatalf("expected invalid port in stderr, got: %s", stderr.String())
		}
	}

	// 4. server start extra argument rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start", "now"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for extra arg, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unexpected argument") {
			t.Fatalf("expected unexpected argument error, got: %s", stderr.String())
		}
	}

	// 5. server start with valid --data-dir and --dataDir (sets TASKVIEW_HOME before Ensure)
	{
		tmpDir := t.TempDir()
		origHome, hadHome := os.LookupEnv("TASKVIEW_HOME")
		defer func() {
			if hadHome {
				_ = os.Setenv("TASKVIEW_HOME", origHome)
			} else {
				_ = os.Unsetenv("TASKVIEW_HOME")
			}
		}()

		var observedEnv string
		lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
			observedEnv = os.Getenv("TASKVIEW_HOME")
			ensurePort = port
			return &ServerState{
				PID:       9988,
				URL:       "http://127.0.0.1:3333",
				DB:        "/test/tasks.db",
				Token:     "SECRET_TOKEN_DO_NOT_LEAK",
				Instance:  "inst123",
				StartedAt: 1700000000000,
			}, nil
		}

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start", "--port", "4321", "--data-dir", tmpDir}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if ensurePort != 4321 {
			t.Fatalf("expected ensurePort 4321, got %d", ensurePort)
		}
		if !strings.EqualFold(observedEnv, tmpDir) {
			t.Fatalf("expected Ensure to observe TASKVIEW_HOME %q, got %q", tmpDir, observedEnv)
		}

		// Camel alias --dataDir
		observedEnv = ""
		stdout.Reset()
		code = RunCLI([]string{"server", "start", "--dataDir", tmpDir}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0 for --dataDir, got %d, stderr: %s", code, stderr.String())
		}
		if !strings.EqualFold(observedEnv, tmpDir) {
			t.Fatalf("expected Ensure to observe TASKVIEW_HOME %q with --dataDir, got %q", tmpDir, observedEnv)
		}

		// Blank --data-dir= rejected
		stdout.Reset()
		stderr.Reset()
		code = RunCLI([]string{"server", "start", "--data-dir="}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for empty data-dir on server start, got %d", code)
		}
		if !strings.Contains(stderr.String(), "data directory cannot be empty") {
			t.Fatalf("expected 'data directory cannot be empty' in stderr, got: %s", stderr.String())
		}

		// Invalid port prevents TASKVIEW_HOME mutation
		_ = os.Setenv("TASKVIEW_HOME", "PRESERVE_FOR_START")
		stderr.Reset()
		code = RunCLI([]string{"server", "start", "--port", "badport", "--data-dir", tmpDir}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for bad port, got %d", code)
		}
		if os.Getenv("TASKVIEW_HOME") != "PRESERVE_FOR_START" {
			t.Fatalf("TASKVIEW_HOME should not be mutated when port validation fails, got: %q", os.Getenv("TASKVIEW_HOME"))
		}
	}

	// 5b. server start unknown flag rejected (e.g. --no-open)
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start", "--no-open"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown flag on server start, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option in stderr, got: %s", stderr.String())
		}
	}

	// 6. server stop
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "stop"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if stopCalled != 1 {
			t.Fatalf("expected stop to be called once, got %d", stopCalled)
		}
		if !strings.Contains(stdout.String(), "server stopped") {
			t.Fatalf("expected server stopped in stdout, got: %s", stdout.String())
		}
	}

	// 7. server stop with unknown flag rejected (port and data-dir rejected)
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "stop", "--port", "3333"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown option on server stop, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option in stderr, got: %s", stderr.String())
		}

		stderr.Reset()
		code = RunCLI([]string{"server", "stop", "--data-dir", "/tmp"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for --data-dir on server stop, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option in stderr, got: %s", stderr.String())
		}
	}

	// 8. server status when running
	{
		statusState = &ServerState{
			PID:       1234,
			URL:       "http://127.0.0.1:4444",
			DB:        "/data/db.sqlite",
			Token:     "LEAK_CHECK_TOKEN",
			Instance:  "inst456",
			StartedAt: 1700000000000,
		}
		statusErr = nil

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "status"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "http://127.0.0.1:4444") || !strings.Contains(out, "PID 1234") || !strings.Contains(out, "/data/db.sqlite") {
			t.Fatalf("expected server status details, got: %s", out)
		}
		if strings.Contains(out, "LEAK_CHECK_TOKEN") {
			t.Fatalf("token must NEVER be printed in stdout, got: %s", out)
		}
	}

	// 9. server status when stopped (nil state returns exit 0 with normal message)
	{
		statusState = nil
		statusErr = nil

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "status"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0 when stopped, got %d", code)
		}
		if !strings.Contains(stdout.String(), "not running") {
			t.Fatalf("expected 'not running' in stdout, got: %s", stdout.String())
		}
	}

	// 10. server status on error (exit nonzero)
	{
		statusState = nil
		statusErr = os.ErrPermission

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "status"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 on status error, got %d", code)
		}
		if !strings.Contains(stderr.String(), "permission denied") {
			t.Fatalf("expected error in stderr, got: %s", stderr.String())
		}

		// server status rejects --data-dir
		stderr.Reset()
		code = RunCLI([]string{"server", "status", "--data-dir", "/tmp"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for --data-dir on server status, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option in stderr, got: %s", stderr.String())
		}
	}

	// 11. server run with --data-dir and --port
	{
		tmpDir := t.TempDir()
		origHome, hadHome := os.LookupEnv("TASKVIEW_HOME")
		defer func() {
			if hadHome {
				_ = os.Setenv("TASKVIEW_HOME", origHome)
			} else {
				_ = os.Unsetenv("TASKVIEW_HOME")
			}
		}()

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "run", "--port", "5555", "--data-dir", tmpDir}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0 for server run, got %d, stderr: %s", code, stderr.String())
		}
		if runPort != 5555 {
			t.Fatalf("expected runPort 5555, got %d", runPort)
		}
		if envHome := os.Getenv("TASKVIEW_HOME"); !strings.EqualFold(envHome, tmpDir) {
			t.Fatalf("expected TASKVIEW_HOME to be %q, got %q", tmpDir, envHome)
		}
	}

	// 12. server start empty --port= rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "start", "--port="}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for empty port in server start, got %d", code)
		}
		if !strings.Contains(stderr.String(), "invalid port") {
			t.Fatalf("expected invalid port in stderr, got: %s", stderr.String())
		}
	}

	// 13. server run empty --port= rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "run", "--port="}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for empty port in server run, got %d", code)
		}
		if !strings.Contains(stderr.String(), "invalid port") {
			t.Fatalf("expected invalid port in stderr, got: %s", stderr.String())
		}
	}

	// 14. server run empty --data-dir= rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "run", "--data-dir="}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for empty data-dir, got %d", code)
		}
		if !strings.Contains(stderr.String(), "data directory cannot be empty") {
			t.Fatalf("expected 'data directory cannot be empty' in stderr, got: %s", stderr.String())
		}
	}

	// 15. server run validates port BEFORE setting TASKVIEW_HOME
	{
		origHome, hadHome := os.LookupEnv("TASKVIEW_HOME")
		defer func() {
			if hadHome {
				_ = os.Setenv("TASKVIEW_HOME", origHome)
			} else {
				_ = os.Unsetenv("TASKVIEW_HOME")
			}
		}()
		_ = os.Setenv("TASKVIEW_HOME", "PRESERVE_ORIGINAL")

		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "run", "--port", "badport", "--data-dir", t.TempDir()}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for bad port, got %d", code)
		}
		if os.Getenv("TASKVIEW_HOME") != "PRESERVE_ORIGINAL" {
			t.Fatalf("TASKVIEW_HOME should not be mutated when port validation fails, got: %q", os.Getenv("TASKVIEW_HOME"))
		}
	}

	// 16. server help / bare invocation
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0 for server help, got %d", code)
		}
		if !strings.Contains(stdout.String(), "taskview server <command>") {
			t.Fatalf("expected server help, got: %s", stdout.String())
		}
	}

	// 17. server unknown subcommand
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"server", "unknownsub"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown server subcommand, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown server command") {
			t.Fatalf("expected unknown server command in stderr, got: %s", stderr.String())
		}
	}
}

func TestCLIUIDispatch(t *testing.T) {
	resetLifecycleHooks(t)

	var ensurePort int
	lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
		ensurePort = port
		return &ServerState{
			PID:       1122,
			URL:       "http://127.0.0.1:3333",
			DB:        "/test/tasks.db",
			Token:     "SECRET",
			Instance:  "inst1",
			StartedAt: 1700000000000,
		}, nil
	}

	var browserURL string
	lifecycleOpenBrowser = func(targetURL string) bool {
		browserURL = targetURL
		return true
	}

	// 1. Default ui invocation (port 3333, opens browser, returns immediately)
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"ui"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if ensurePort != 3333 {
			t.Fatalf("expected ensurePort 3333, got %d", ensurePort)
		}
		if browserURL != "http://127.0.0.1:3333" {
			t.Fatalf("expected browser opened to http://127.0.0.1:3333, got %q", browserURL)
		}
		out := stdout.String()
		if !strings.Contains(out, "http://127.0.0.1:3333") || !strings.Contains(out, "TaskView") {
			t.Fatalf("expected UI running output, got: %s", out)
		}
		if strings.Contains(out, "Ctrl+C") {
			t.Fatalf("background UI server should not ask user to press Ctrl+C: %s", out)
		}
	}

	// 2. ui with --no-open
	{
		browserURL = ""
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"ui", "--no-open"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if browserURL != "" {
			t.Fatalf("expected browser NOT to be opened with --no-open, got: %s", browserURL)
		}
	}

	// 3. ui with --port 0 (smoke test for port 0)
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"ui", "--port", "0"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
		}
		if ensurePort != 0 {
			t.Fatalf("expected port 0, got %d", ensurePort)
		}
	}

	// 4. ui empty --port= rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"ui", "--port="}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for empty port in ui, got %d", code)
		}
		if !strings.Contains(stderr.String(), "invalid port") {
			t.Fatalf("expected invalid port in stderr, got: %s", stderr.String())
		}
	}

	// 5. ui extra argument rejected
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"ui", "extra-arg"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for extra arg, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unexpected argument") {
			t.Fatalf("expected unexpected argument error, got: %s", stderr.String())
		}

		// ui rejects --data-dir
		stderr.Reset()
		code = RunCLI([]string{"ui", "--data-dir", "/tmp"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for --data-dir on ui, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown option") {
			t.Fatalf("expected unknown option in stderr, got: %s", stderr.String())
		}
	}
}

func TestCLINoImplicitInstallFromTaskCommands(t *testing.T) {
	resetLifecycleHooks(t)
	store := newTestStore(t)

	var installCalls int
	lifecycleInstall = func(opts InstallOptions) (*InstallResult, error) {
		installCalls++
		return nil, nil
	}

	// Running task commands should NEVER implicitly call Install
	var stdout, stderr bytes.Buffer
	proj, projName := newProject(t, "alpha")
	code := RunCLI(append([]string{"add", "Task without install"}, projectFlags(proj, projName)...), store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("failed to add task: %s", stderr.String())
	}
	if installCalls != 0 {
		t.Fatalf("add command should never call Install, got %d calls", installCalls)
	}

	code = RunCLI(append([]string{"list"}, projectFlags(proj, projName)...), store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("failed to list tasks: %s", stderr.String())
	}
	if installCalls != 0 {
		t.Fatalf("list command should never call Install, got %d calls", installCalls)
	}

	tasks, _ := store.ListTasks(ListOptions{All: true})
	if len(tasks) > 0 {
		code = RunCLI([]string{"update", tasks[0].ID, "--status", "done"}, store, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("failed to update task: %s", stderr.String())
		}
		if installCalls != 0 {
			t.Fatalf("update command should never call Install, got %d calls", installCalls)
		}
	}
}

func TestCLINoDatabaseAccessForServiceCommands(t *testing.T) {
	resetLifecycleHooks(t)

	lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
		return &ServerState{PID: 123, URL: "http://127.0.0.1:3333"}, nil
	}
	lifecycleManagedServerStatus = func() (*ServerState, error) {
		return nil, nil
	}
	lifecycleStopBackgroundServer = func() error {
		return nil
	}
	lifecycleInstall = func(opts InstallOptions) (*InstallResult, error) {
		return &InstallResult{Path: "/test/taskview.exe"}, nil
	}
	lifecycleUninstall = func() error {
		return nil
	}
	lifecycleOpenBrowser = func(url string) bool {
		return true
	}

	// Passing nil store to RunCLI for non-task commands must succeed without touching database
	cases := [][]string{
		{"help"},
		{"version"},
		{"install"},
		{"uninstall"},
		{"server", "status"},
		{"server", "start"},
		{"server", "stop"},
		{"ui", "--no-open"},
	}

	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := RunCLI(args, nil, &stdout, &stderr)
		if code != 0 {
			t.Errorf("args %v expected code 0 with nil store, got %d, stderr: %s", args, code, stderr.String())
		}
	}
}

func TestCLIRunMainNoDatabaseOpenOnServiceCommands(t *testing.T) {
	resetLifecycleHooks(t)

	lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
		return &ServerState{PID: 123, URL: "http://127.0.0.1:3333"}, nil
	}
	lifecycleManagedServerStatus = func() (*ServerState, error) {
		return nil, nil
	}
	lifecycleStopBackgroundServer = func() error {
		return nil
	}
	lifecycleInstall = func(opts InstallOptions) (*InstallResult, error) {
		return &InstallResult{Path: "/test/taskview.exe"}, nil
	}
	lifecycleUninstall = func() error {
		return nil
	}
	lifecycleOpenBrowser = func(url string) bool {
		return true
	}

	// Point TASKVIEW_HOME to an invalid database location (a file instead of dir)
	// so that if OpenStore("") were called, it would fail.
	tmpFile := filepath.Join(t.TempDir(), "dummy_file")
	if err := os.WriteFile(tmpFile, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("failed to create dummy file: %v", err)
	}

	origHome := os.Getenv("TASKVIEW_HOME")
	defer func() {
		_ = os.Setenv("TASKVIEW_HOME", origHome)
	}()
	_ = os.Setenv("TASKVIEW_HOME", tmpFile)

	serviceCommands := [][]string{
		{"version"},
		{"help"},
		{"install"},
		{"uninstall"},
		{"server", "status"},
		{"server", "start"},
		{"server", "stop"},
		{"ui", "--no-open"},
	}

	for _, args := range serviceCommands {
		code := runMain(args)
		if code != 0 {
			t.Errorf("runMain(%v) failed with code %d when DB was invalid; service commands must not open DB", args, code)
		}
	}

	// Unknown commands should exit 1 with unknown command error, not fail trying to open DB
	{
		var stdout, stderr bytes.Buffer
		code := RunCLI([]string{"boguscommand"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown command, got %d", code)
		}
		if !strings.Contains(stderr.String(), "unknown command") {
			t.Fatalf("expected 'unknown command' in stderr, got: %s", stderr.String())
		}
	}
}

func TestEnsureServerRunningLifecycle(t *testing.T) {
	resetLifecycleHooks(t)
	t.Setenv("TASKVIEW_HOME", t.TempDir())

	t.Run("already running skips the start", func(t *testing.T) {
		var ensureCalls int
		lifecycleManagedServerStatus = func() (*ServerState, error) {
			return &ServerState{PID: 123, URL: "http://127.0.0.1:3333"}, nil
		}
		lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
			ensureCalls++
			return &ServerState{PID: 123, URL: "http://127.0.0.1:3333"}, nil
		}

		var stdout, stderr bytes.Buffer
		if !ensureServerRunning(&stdout, &stderr, ColorConfig{}) {
			t.Fatalf("expected ensureServerRunning to succeed, stderr: %s", stderr.String())
		}
		if ensureCalls != 0 {
			t.Fatalf("expected no start when the server is already running, got %d calls", ensureCalls)
		}
		if stdout.Len() != 0 {
			t.Errorf("expected empty stdout, got: %s", stdout.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("expected no stderr output when the server is already running, got: %s", stderr.String())
		}
	})

	t.Run("not running starts the server", func(t *testing.T) {
		var ensureCalls int
		var ensuredPort int
		lifecycleManagedServerStatus = func() (*ServerState, error) {
			return nil, nil
		}
		lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
			ensureCalls++
			ensuredPort = port
			return &ServerState{PID: 4242, URL: "http://127.0.0.1:3333"}, nil
		}

		var stdout, stderr bytes.Buffer
		c := ColorConfig{Dim: "\x1b[2m", Reset: "\x1b[0m"}
		if !ensureServerRunning(&stdout, &stderr, c) {
			t.Fatalf("expected ensureServerRunning to succeed, stderr: %s", stderr.String())
		}
		if ensureCalls != 1 {
			t.Fatalf("expected exactly 1 ensure call, got %d", ensureCalls)
		}
		if ensuredPort != 3333 {
			t.Errorf("expected port 3333, got %d", ensuredPort)
		}
		if stdout.Len() != 0 {
			t.Errorf("expected stdout to stay empty, got: %s", stdout.String())
		}
		if !strings.Contains(stderr.String(), "Started TaskView server at http://127.0.0.1:3333 (PID 4242)") {
			t.Errorf("expected the start notice on stderr, got: %s", stderr.String())
		}
		if !strings.HasPrefix(stderr.String(), c.Dim) {
			t.Errorf("expected a dim start line, got: %q", stderr.String())
		}
	})

	t.Run("start failure reports an error", func(t *testing.T) {
		lifecycleManagedServerStatus = func() (*ServerState, error) {
			return nil, nil
		}
		lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
			return nil, errors.New("spawn failed")
		}

		var stdout, stderr bytes.Buffer
		if ensureServerRunning(&stdout, &stderr, ColorConfig{}) {
			t.Fatalf("expected ensureServerRunning to fail")
		}
		if stdout.Len() != 0 {
			t.Errorf("expected stdout to stay empty, got: %s", stdout.String())
		}
		if !strings.Contains(stderr.String(), "Error:") || !strings.Contains(stderr.String(), "failed to ensure background server") {
			t.Errorf("expected a clear error on stderr, got: %s", stderr.String())
		}
	})

	t.Run("status probe failure reports an error", func(t *testing.T) {
		lifecycleManagedServerStatus = func() (*ServerState, error) {
			return nil, errors.New("state unreadable")
		}
		lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
			t.Errorf("ensure must not be called when the status probe fails")
			return nil, nil
		}

		var stdout, stderr bytes.Buffer
		if ensureServerRunning(&stdout, &stderr, ColorConfig{}) {
			t.Fatalf("expected ensureServerRunning to fail")
		}
		if !strings.Contains(stderr.String(), "Error:") {
			t.Errorf("expected a clear error on stderr, got: %s", stderr.String())
		}
	})
}

func TestRunCLIDataCommandsStopBeforeDatabaseOnServerFailure(t *testing.T) {
	for _, cmd := range []string{"add", "list", "get", "update", "delete", "stats", "projects"} {
		t.Run(cmd, func(t *testing.T) {
			resetLifecycleHooks(t)
			home := filepath.Join(t.TempDir(), "unopened")
			t.Setenv("TASKVIEW_HOME", home)
			var statusCalls, ensureCalls int
			lifecycleManagedServerStatus = func() (*ServerState, error) {
				statusCalls++
				return nil, nil
			}
			lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
				ensureCalls++
				return nil, errors.New("spawn failed")
			}

			var stdout, stderr bytes.Buffer
			if code := RunCLI([]string{cmd}, nil, &stdout, &stderr); code != 1 {
				t.Fatalf("expected exit 1, got %d", code)
			}
			if statusCalls != 1 || ensureCalls != 1 {
				t.Fatalf("expected one probe and one start, got %d/%d", statusCalls, ensureCalls)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "failed to ensure background server: spawn failed") {
				t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("database directory must remain absent, got %v", err)
			}
		})
	}
}

func TestRunCLIDataCommandEnsuresBackgroundServer(t *testing.T) {
	resetLifecycleHooks(t)
	t.Setenv("TASKVIEW_HOME", t.TempDir())

	var statusCalls, ensureCalls int
	lifecycleManagedServerStatus = func() (*ServerState, error) {
		statusCalls++
		return nil, nil
	}
	lifecycleEnsureBackgroundServer = func(port int) (*ServerState, error) {
		ensureCalls++
		return &ServerState{PID: 4321, URL: "http://127.0.0.1:3333"}, nil
	}
	lifecycleStopBackgroundServer = func() error { return nil }
	lifecycleInstall = func(opts InstallOptions) (*InstallResult, error) {
		return &InstallResult{Path: filepath.Join(t.TempDir(), "taskview.exe")}, nil
	}
	lifecycleUninstall = func() error { return nil }
	lifecycleOpenBrowser = func(url string) bool { return false }

	// A data command without an injected store goes through the ensure path.
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"projects"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
	}
	if statusCalls != 1 || ensureCalls != 1 {
		t.Fatalf("expected 1 status probe and 1 ensure call, got %d/%d", statusCalls, ensureCalls)
	}
	if !strings.Contains(stderr.String(), "Started TaskView server") {
		t.Errorf("expected the start notice on stderr, got: %s", stderr.String())
	}

	// Commands that manage the server (or need none) never probe it.
	for _, args := range [][]string{
		{"help"},
		{"version"},
		{"ui", "--no-open"},
		{"server", "stop"},
		{"install"},
		{"uninstall"},
	} {
		statusCalls, ensureCalls = 0, 0
		stdout.Reset()
		stderr.Reset()
		if code := RunCLI(args, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("args %v expected code 0, got %d, stderr: %s", args, code, stderr.String())
		}
		if statusCalls != 0 {
			t.Errorf("args %v must not probe the background server, got %d probes", args, statusCalls)
		}
	}
}
