// Unit tests for the TaskView store. They never touch the real user database:
// every test opens a database under t.TempDir() (or uses :memory:).

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// taskIDPattern is the new ordered task id shape: T + YYYYMMDD + 4-digit seq.
var taskIDPattern = regexp.MustCompile(`^T[0-9]{8}[0-9]{4}$`)

func mustOpen(t *testing.T, filename string) *Store {
	t.Helper()
	store, err := OpenStore(filename)
	if err != nil {
		t.Fatalf("OpenStore(%q): %v", filename, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func memStore(t *testing.T) *Store {
	t.Helper()
	return mustOpen(t, ":memory:")
}

func create(t *testing.T, store *Store, data map[string]any) *Task {
	t.Helper()
	task, err := store.CreateTask(data)
	if err != nil {
		t.Fatalf("CreateTask(%v): %v", data, err)
	}
	return task
}

// ----------------------------------------------------------------- basics --

func TestDefaultDBPathHonoursTaskviewHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKVIEW_HOME", dir)

	got, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	want := filepath.Join(dir, "tasks.db")
	if got != want {
		t.Fatalf("DefaultDBPath() = %q, want %q", got, want)
	}
}

func TestDefaultDBPathFallsBackToHome(t *testing.T) {
	t.Setenv("TASKVIEW_HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	got, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	want := filepath.Join(home, ".taskview", "tasks.db")
	if got != want {
		t.Fatalf("DefaultDBPath() = %q, want %q", got, want)
	}
}

func TestOpenStoreCreatesDirectoryAndFile(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "deeply", "nested", "tasks.db")

	store := mustOpen(t, nested)

	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	if store.FilePath == "" {
		t.Fatal("Store.FilePath is empty for an on-disk database")
	}
	if !filepath.IsAbs(store.FilePath) {
		t.Fatalf("Store.FilePath = %q, want absolute path", store.FilePath)
	}
}

func TestOpenStoreRetainsSchemaAndIndexes(t *testing.T) {
	store := memStore(t)

	// Note: the pool is limited to one connection, so every rows cursor must be
	// fully drained and closed before the next query.
	table := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='tasks'`)
	var tableName string
	if err := table.Scan(&tableName); err != nil {
		t.Fatalf("tasks table missing: %v", err)
	}

	indexes, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND name LIKE 'idx_tasks_%'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	found := map[string]bool{}
	for indexes.Next() {
		var name string
		if err := indexes.Scan(&name); err != nil {
			indexes.Close()
			t.Fatalf("scan index: %v", err)
		}
		found[name] = true
	}
	if err := indexes.Err(); err != nil {
		indexes.Close()
		t.Fatalf("iterate indexes: %v", err)
	}
	indexes.Close()

	for _, want := range []string{"idx_tasks_project_path", "idx_tasks_status", "idx_tasks_created_at"} {
		if !found[want] {
			t.Errorf("missing index %s (found: %v)", want, found)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Operations after Close must fail loudly rather than panic.
	if _, err := store.GetTask("anything"); err == nil {
		t.Fatal("GetTask after Close should return an error")
	}
}

// ------------------------------------------------------------------ CRUD --

func TestCreateTaskAppliesDefaults(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Default task", "project_path": t.TempDir()})

	if !taskIDPattern.MatchString(task.ID) {
		t.Errorf("ID = %q, want T<YYYYMMDD><NNNN>", task.ID)
	}
	if task.Status != "todo" {
		t.Errorf("Status = %q, want todo", task.Status)
	}
	if task.Priority != "medium" {
		t.Errorf("Priority = %q, want medium", task.Priority)
	}
	if task.ClosedAt != nil {
		t.Errorf("ClosedAt = %v, want nil", *task.ClosedAt)
	}
	if task.Description != nil || task.FilePath != nil ||
		task.AgentNotes != nil || task.CodeSnippet != nil ||
		task.LineStart != nil || task.LineEnd != nil {
		t.Error("optional fields should default to NULL")
	}
	if task.CreatedAt <= 0 || task.UpdatedAt != task.CreatedAt {
		t.Errorf("timestamps: created=%d updated=%d", task.CreatedAt, task.UpdatedAt)
	}
	if !filepath.IsAbs(task.ProjectPath) {
		t.Errorf("ProjectPath = %q, want absolute", task.ProjectPath)
	}
	if task.ProjectName != filepath.Base(task.ProjectPath) {
		t.Errorf("ProjectName = %q, want basename of %q", task.ProjectName, task.ProjectPath)
	}
}

func TestCreateTaskResolvesProjectFromWorkingDirectory(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Auto project"})

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if filepath.Clean(task.ProjectPath) != filepath.Clean(cwd) {
		t.Errorf("ProjectPath = %q, want %q", task.ProjectPath, cwd)
	}
	if task.ProjectName != filepath.Base(cwd) {
		t.Errorf("ProjectName = %q, want %q", task.ProjectName, filepath.Base(cwd))
	}
}

func TestCreateTaskRequiresTitle(t *testing.T) {
	store := memStore(t)
	for _, title := range []any{nil, "", "   "} {
		_, err := store.CreateTask(map[string]any{"title": title})
		if !IsValidationError(err) {
			t.Errorf("title %v: err = %v, want ValidationError", title, err)
		}
	}
	if _, err := store.CreateTask(nil); !IsValidationError(err) {
		t.Errorf("nil map: err = %v, want ValidationError", err)
	}
}

func TestCreateTaskValidatesStatusAndPriority(t *testing.T) {
	store := memStore(t)

	if _, err := store.CreateTask(map[string]any{"title": "x", "status": "nope"}); !IsValidationError(err) {
		t.Errorf("bad status: err = %v, want ValidationError", err)
	}
	if _, err := store.CreateTask(map[string]any{"title": "x", "priority": "nope"}); !IsValidationError(err) {
		t.Errorf("bad priority: err = %v, want ValidationError", err)
	}
}

func TestCreateTaskWithDoneStatusSetsClosedAt(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Already done", "status": "done"})

	if task.Status != "done" {
		t.Fatalf("Status = %q, want done", task.Status)
	}
	if task.ClosedAt == nil {
		t.Fatal("ClosedAt should be set when a task is created as done")
	}
	if *task.ClosedAt != task.CreatedAt {
		t.Errorf("ClosedAt = %d, want %d", *task.ClosedAt, task.CreatedAt)
	}
}

func TestGetTaskReturnsNilForMissingID(t *testing.T) {
	store := memStore(t)
	task, err := store.GetTask("task_does_not_exist")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task != nil {
		t.Fatalf("GetTask returned %v, want nil", task)
	}
}

func TestDeleteTaskReportsWhetherRowWasRemoved(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Doomed"})

	ok, err := store.DeleteTask(task.ID)
	if err != nil || !ok {
		t.Fatalf("DeleteTask = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = store.DeleteTask(task.ID)
	if err != nil {
		t.Fatalf("second DeleteTask: %v", err)
	}
	if ok {
		t.Error("DeleteTask on a missing row should return false")
	}
}

// ---------------------------------------------------------------- update --

func TestUpdateTaskChangesFieldsAndBumpsUpdatedAt(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Updatable"})

	time.Sleep(2 * time.Millisecond)
	updated, err := store.UpdateTask(task.ID, map[string]any{"status": "in_progress", "priority": "urgent"})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Status != "in_progress" || updated.Priority != "urgent" {
		t.Errorf("got %s/%s, want in_progress/urgent", updated.Status, updated.Priority)
	}
	if updated.UpdatedAt <= task.UpdatedAt {
		t.Errorf("UpdatedAt %d did not advance past %d", updated.UpdatedAt, task.UpdatedAt)
	}
	if updated.ClosedAt != nil {
		t.Error("ClosedAt should stay NULL while the task is not done")
	}
}

func TestUpdateTaskMaintainsClosedAt(t *testing.T) {
	store := memStore(t)

	task := create(t, store, map[string]any{"title": "Closable"})

	done, err := store.UpdateTask(task.ID, map[string]any{"status": "done"})
	if err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if done.ClosedAt == nil {
		t.Fatal("ClosedAt should be set when moving to done")
	}
	if *done.ClosedAt < done.CreatedAt {
		t.Errorf("ClosedAt %d predates CreatedAt %d", *done.ClosedAt, done.CreatedAt)
	}

	// Updating a done task without touching status must not move ClosedAt.
	again, err := store.UpdateTask(task.ID, map[string]any{"title": "Closable v2"})
	if err != nil {
		t.Fatalf("retitle: %v", err)
	}
	if again.ClosedAt == nil || *again.ClosedAt != *done.ClosedAt {
		t.Errorf("ClosedAt changed on unrelated update: %v -> %v", *done.ClosedAt, again.ClosedAt)
	}

	reopened, err := store.UpdateTask(task.ID, map[string]any{"status": "in_progress"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.ClosedAt != nil {
		t.Errorf("ClosedAt = %d, want NULL after reopening", *reopened.ClosedAt)
	}
}

func TestUpdateTaskAcceptsCamelCaseAliases(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Camel"})

	updated, err := store.UpdateTask(task.ID, map[string]any{
		"agentNotes":  "note",
		"filePath":    "a.js",
		"lineStart":   float64(3),
		"codeSnippet": "func main() {}",
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.AgentNotes == nil || *updated.AgentNotes != "note" {
		t.Errorf("AgentNotes = %v, want note", updated.AgentNotes)
	}
	if updated.FilePath == nil || *updated.FilePath != "a.js" {
		t.Errorf("FilePath = %v, want a.js", updated.FilePath)
	}
	if updated.LineStart == nil || *updated.LineStart != 3 {
		t.Errorf("LineStart = %v, want 3", updated.LineStart)
	}
	if updated.CodeSnippet == nil {
		t.Error("CodeSnippet should be set")
	}
}

func TestCreateAcceptsSnakeAndCamelAliases(t *testing.T) {
	store := memStore(t)

	snake := create(t, store, map[string]any{
		"title":        "snake",
		"file_path":    "snake.go",
		"line_start":   float64(4),
		"line_end":     float64(8),
		"code_snippet": "x",
		"agent_notes":  "n",
	})
	camel := create(t, store, map[string]any{
		"title":       "camel",
		"filePath":    "camel.go",
		"lineStart":   float64(4),
		"lineEnd":     float64(8),
		"codeSnippet": "x",
		"agentNotes":  "n",
	})

	for name, task := range map[string]*Task{"snake": snake, "camel": camel} {
		if task.FilePath == nil || *task.FilePath != name+".go" {
			t.Errorf("%s FilePath = %v", name, task.FilePath)
		}
		if task.LineStart == nil || *task.LineStart != 4 || task.LineEnd == nil || *task.LineEnd != 8 {
			t.Errorf("%s line range = %v..%v, want 4..8", name, task.LineStart, task.LineEnd)
		}
	}
}

func TestCreateAcceptsStringAndFloatNumbers(t *testing.T) {
	store := memStore(t)

	fromString := create(t, store, map[string]any{"title": "str", "line_start": "12"})
	if fromString.LineStart == nil || *fromString.LineStart != 12 {
		t.Errorf("string line_start = %v, want 12", fromString.LineStart)
	}
	fromFloat := create(t, store, map[string]any{"title": "float", "line_start": float64(34)})
	if fromFloat.LineStart == nil || *fromFloat.LineStart != 34 {
		t.Errorf("float line_start = %v, want 34", fromFloat.LineStart)
	}
	fromInt := create(t, store, map[string]any{"title": "int", "line_start": 56})
	if fromInt.LineStart == nil || *fromInt.LineStart != 56 {
		t.Errorf("int line_start = %v, want 56", fromInt.LineStart)
	}
}

func TestUpdateTaskClearsNullableFields(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{
		"title":        "Clearable",
		"description":  "desc",
		"file_path":    "a.go",
		"line_start":   float64(1),
		"agent_notes":  "note",
		"code_snippet": "code",
	})

	updated, err := store.UpdateTask(task.ID, map[string]any{
		"description":  nil,
		"file_path":    nil,
		"line_start":   nil,
		"agent_notes":  nil,
		"code_snippet": nil,
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Description != nil || updated.FilePath != nil || updated.LineStart != nil ||
		updated.AgentNotes != nil || updated.CodeSnippet != nil {
		t.Errorf("explicit nulls should clear fields, got %+v", updated)
	}
}

func TestUpdateTaskIgnoresUnknownFieldsButRequiresOneKnown(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Known"})

	if _, err := store.UpdateTask(task.ID, map[string]any{"bogus": 1, "id": "x"}); !IsValidationError(err) {
		t.Errorf("unknown fields only: err = %v, want ValidationError", err)
	}
	if _, err := store.UpdateTask(task.ID, map[string]any{}); !IsValidationError(err) {
		t.Errorf("empty map: err = %v, want ValidationError", err)
	}

	updated, err := store.UpdateTask(task.ID, map[string]any{"bogus": 1, "title": "Renamed"})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Title != "Renamed" {
		t.Errorf("Title = %q, want Renamed", updated.Title)
	}
	if updated.ID != task.ID {
		t.Errorf("ID changed to %q, want %q", updated.ID, task.ID)
	}
}

func TestUpdateTaskValidatesTitleAndEnums(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Validate me"})

	if _, err := store.UpdateTask(task.ID, map[string]any{"title": "  "}); !IsValidationError(err) {
		t.Errorf("blank title: err = %v, want ValidationError", err)
	}
	if _, err := store.UpdateTask(task.ID, map[string]any{"status": "nope"}); !IsValidationError(err) {
		t.Errorf("bad status: err = %v, want ValidationError", err)
	}
	if _, err := store.UpdateTask(task.ID, map[string]any{"priority": "nope"}); !IsValidationError(err) {
		t.Errorf("bad priority: err = %v, want ValidationError", err)
	}
}

func TestUpdateTaskReturnsNilForMissingID(t *testing.T) {
	store := memStore(t)
	task, err := store.UpdateTask("task_missing", map[string]any{"title": "x"})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if task != nil {
		t.Fatalf("UpdateTask returned %v, want nil", task)
	}
}

func TestUpdateTaskRejectsReversedLineRange(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Ranged", "line_start": float64(10), "line_end": float64(20)})

	if _, err := store.UpdateTask(task.ID, map[string]any{"line_end": float64(5)}); !IsValidationError(err) {
		t.Errorf("reversed range: err = %v, want ValidationError", err)
	}
	if _, err := store.UpdateTask(task.ID, map[string]any{"line_start": float64(30), "line_end": float64(1)}); !IsValidationError(err) {
		t.Errorf("reversed both: err = %v, want ValidationError", err)
	}
	// Clearing only the start must not be treated as a reversal.
	if _, err := store.UpdateTask(task.ID, map[string]any{"line_start": nil}); err != nil {
		t.Errorf("clearing line_start: %v", err)
	}
}

func TestAppendNotes(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Notes"})

	if _, err := store.AppendNotes(task.ID, "first"); err != nil {
		t.Fatalf("AppendNotes: %v", err)
	}
	appended, err := store.AppendNotes(task.ID, "second")
	if err != nil {
		t.Fatalf("AppendNotes: %v", err)
	}
	if appended.AgentNotes == nil || *appended.AgentNotes != "first\nsecond" {
		t.Errorf("AgentNotes = %v, want \"first\\nsecond\"", appended.AgentNotes)
	}

	missing, err := store.AppendNotes("task_missing", "x")
	if err != nil || missing != nil {
		t.Errorf("AppendNotes on missing id = (%v, %v), want (nil, nil)", missing, err)
	}
}

// TestAppendNotesIsAtomicUnderConcurrency guards the append against the
// read-modify-write race: every note has to survive, no matter how many
// writers append at the same time.
func TestAppendNotesIsAtomicUnderConcurrency(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Concurrent notes"})

	const (
		writers   = 8
		perWriter = 5
	)

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for worker := 0; worker < writers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := store.AppendNotes(task.ID, fmt.Sprintf("note-%d-%d", worker, i)); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendNotes: %v", err)
	}

	updated, err := store.GetTask(task.ID)
	if err != nil || updated == nil {
		t.Fatalf("GetTask: (%v, %v)", updated, err)
	}
	if updated.AgentNotes == nil {
		t.Fatal("AgentNotes is nil, want the appended notes")
	}
	lines := strings.Split(*updated.AgentNotes, "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("got %d notes, want %d - an append was lost", len(lines), writers*perWriter)
	}
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		seen[line] = true
	}
	for worker := 0; worker < writers; worker++ {
		for i := 0; i < perWriter; i++ {
			want := fmt.Sprintf("note-%d-%d", worker, i)
			if !seen[want] {
				t.Errorf("note %q is missing", want)
			}
		}
	}
}

// TestAppendNotesKeepsBusinessUpdateLogic checks that appending a note
// behaves like any other update: the modification time moves, the closing
// time stays as it is.
func TestAppendNotesKeepsBusinessUpdateLogic(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Done task", "status": "done"})
	if task.ClosedAt == nil {
		t.Fatal("a task created as done must have a closed_at")
	}
	closedAt := *task.ClosedAt

	// Make the timestamp change observable.
	time.Sleep(2 * time.Millisecond)
	updated, err := store.AppendNotes(task.ID, "a note")
	if err != nil {
		t.Fatalf("AppendNotes: %v", err)
	}
	if updated.UpdatedAt <= task.UpdatedAt {
		t.Errorf("UpdatedAt = %d, want more than %d", updated.UpdatedAt, task.UpdatedAt)
	}
	if updated.ClosedAt == nil || *updated.ClosedAt != closedAt {
		t.Errorf("ClosedAt = %v, want %v - appending a note must not close a task", updated.ClosedAt, closedAt)
	}
	if updated.Status != "done" {
		t.Errorf("Status = %q, want done", updated.Status)
	}
}

// ------------------------------------------------------------ validation --

func TestLineNumberValidation(t *testing.T) {
	store := memStore(t)

	bad := []any{float64(1.5), "abc", -3, float64(-1), true}
	for _, value := range bad {
		_, err := store.CreateTask(map[string]any{"title": "bad line", "line_start": value})
		if !IsValidationError(err) {
			t.Errorf("line_start %v (%T): err = %v, want ValidationError", value, value, err)
		}
	}

	if _, err := store.CreateTask(map[string]any{"title": "reversed", "line_start": float64(30), "line_end": float64(1)}); !IsValidationError(err) {
		t.Errorf("reversed range: err = %v, want ValidationError", err)
	}
}

func TestParseLineRange(t *testing.T) {
	cases := []struct {
		input     string
		wantStart *int64
		wantEnd   *int64
	}{
		{"12-30", ptr(12), ptr(30)},
		{"42", ptr(42), ptr(42)},
		{"7-", ptr(7), nil},
		{"12 - 30", ptr(12), ptr(30)},
		{"", nil, nil},
		{"  ", nil, nil},
	}
	for _, tc := range cases {
		start, end, err := ParseLineRange(tc.input)
		if err != nil {
			t.Errorf("ParseLineRange(%q): %v", tc.input, err)
			continue
		}
		if !equalIntPtr(start, tc.wantStart) || !equalIntPtr(end, tc.wantEnd) {
			t.Errorf("ParseLineRange(%q) = %v..%v, want %v..%v", tc.input, start, end, tc.wantStart, tc.wantEnd)
		}
	}

	// "-4" parses as a bare negative line number, which is rejected rather than
	// being read as an open-ended range.
	for _, bad := range []string{"abc", "1.5", "--", "3-1-2x", "-4", "-1-2"} {
		if _, _, err := ParseLineRange(bad); !IsValidationError(err) {
			t.Errorf("ParseLineRange(%q): err = %v, want ValidationError", bad, err)
		}
	}
}

func TestCreateTaskIDsUseDailySequence(t *testing.T) {
	store := memStore(t)

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		task := create(t, store, map[string]any{"title": fmt.Sprintf("Sequenced %d", i)})
		if !taskIDPattern.MatchString(task.ID) {
			t.Fatalf("task %d: ID = %q, want T<YYYYMMDD><NNNN>", i, task.ID)
		}
		ids = append(ids, task.ID)
	}

	// All three share one date, taken from the ids themselves rather than from
	// time.Now(), so the test cannot flake across midnight.
	date := ids[0][1:9]
	for i, id := range ids {
		if id[1:9] != date {
			t.Errorf("task %d: date = %q, want %q (id %q)", i, id[1:9], date, id)
		}
		wantSuffix := fmt.Sprintf("%04d", i+1)
		if id[9:] != wantSuffix {
			t.Errorf("task %d: suffix = %q, want %q (id %q)", i, id[9:], wantSuffix, id)
		}
	}
}

// TestCreateTaskIDsUniqueAcrossStores exercises the allocation loop against a
// real file with two independent handles, the way a daemon and a CLI can race.
func TestCreateTaskIDsUniqueAcrossStores(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tasks.db")
	storeA := mustOpen(t, dbPath)
	storeB := mustOpen(t, dbPath)

	const perStore = 10
	ids := make([]string, 0, 2*perStore)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, 2*perStore)

	for _, store := range []*Store{storeA, storeB} {
		for i := 0; i < perStore; i++ {
			wg.Add(1)
			go func(store *Store, i int) {
				defer wg.Done()
				task, err := store.CreateTask(map[string]any{
					"title": fmt.Sprintf("concurrent %d", i),
				})
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				ids = append(ids, task.ID)
				mu.Unlock()
			}(store, i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("CreateTask: %v", err)
	}

	if len(ids) != 2*perStore {
		t.Fatalf("created %d tasks, want %d", len(ids), 2*perStore)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !taskIDPattern.MatchString(id) {
			t.Errorf("id = %q, want T<YYYYMMDD><NNNN>", id)
		}
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 2*perStore {
		t.Errorf("%d distinct ids, want %d", len(seen), 2*perStore)
	}

	var count int
	if err := storeA.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2*perStore {
		t.Errorf("row count = %d, want %d", count, 2*perStore)
	}
}

func TestTaskIDHelpers(t *testing.T) {
	when := time.Date(2026, time.October, 19, 23, 59, 59, 0, time.UTC)
	prefix := taskIDPrefix(when)
	if prefix != "T20261019" {
		t.Errorf("taskIDPrefix = %q, want T20261019", prefix)
	}
	if got := formatTaskID(prefix, 1); got != "T202610190001" {
		t.Errorf("formatTaskID(prefix, 1) = %q, want T202610190001", got)
	}
	if got := formatTaskID(prefix, 9999); got != "T202610199999" {
		t.Errorf("formatTaskID(prefix, 9999) = %q, want T202610199999", got)
	}
}

func TestValidationErrorMessage(t *testing.T) {
	_, err := memStore(t).CreateTask(map[string]any{"title": ""})
	if !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatal("errors.As should extract *ValidationError")
	}
	if !strings.Contains(ve.Message, "title") {
		t.Errorf("Message = %q, want it to mention title", ve.Message)
	}
	if ve.Error() != ve.Message {
		t.Errorf("Error() = %q, want %q", ve.Error(), ve.Message)
	}
}

// ----------------------------------------------------------- list & query --

func TestListTasksProjectIsolation(t *testing.T) {
	store := memStore(t)
	alphaPath := t.TempDir()
	betaPath := t.TempDir()

	create(t, store, map[string]any{"title": "A1", "project_path": alphaPath, "project_name": "alpha"})
	create(t, store, map[string]any{"title": "A2", "project_path": alphaPath, "project_name": "alpha"})
	create(t, store, map[string]any{"title": "B1", "project_path": betaPath, "project_name": "beta"})

	scoped, err := store.ListTasks(ListOptions{ProjectPath: alphaPath})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(scoped) != 2 {
		t.Errorf("alpha tasks = %d, want 2", len(scoped))
	}

	all, err := store.ListTasks(ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListTasks(all): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all tasks = %d, want 3", len(all))
	}

	other := t.TempDir()
	empty, err := store.ListTasks(ListOptions{ProjectPath: other})
	if err != nil {
		t.Fatalf("ListTasks(unused project): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unused project = %d tasks, want 0", len(empty))
	}

	// The default scope is the current working directory, which holds no tasks.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defaultScope, err := store.ListTasks(ListOptions{})
	if err != nil {
		t.Fatalf("ListTasks(default): %v", err)
	}
	if len(defaultScope) != 0 {
		t.Errorf("default scope (%s) = %d tasks, want 0", cwd, len(defaultScope))
	}
}

func TestListTasksFilters(t *testing.T) {
	store := memStore(t)
	dir := t.TempDir()
	base := map[string]any{"project_path": dir, "project_name": "proj"}
	with := func(extra map[string]any) {
		merged := map[string]any{"title": "t"}
		for k, v := range base {
			merged[k] = v
		}
		for k, v := range extra {
			merged[k] = v
		}
		create(t, store, merged)
	}
	with(map[string]any{"title": "Fix auth bug", "description": "login breaks", "priority": "urgent", "status": "todo"})
	with(map[string]any{"title": "Write docs", "priority": "low", "status": "done", "file_path": "README.md"})

	count := func(opts ListOptions, want int, label string) {
		t.Helper()
		got, err := store.ListTasks(opts)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(got) != want {
			t.Errorf("%s = %d tasks, want %d", label, len(got), want)
		}
	}

	count(ListOptions{ProjectPath: dir, Status: "done"}, 1, "status=done")
	count(ListOptions{ProjectPath: dir, Status: "DONE"}, 1, "status case-insensitive")
	count(ListOptions{ProjectPath: dir, Priority: "urgent"}, 1, "priority=urgent")
	count(ListOptions{ProjectPath: dir, Search: "auth"}, 1, "search title")
	count(ListOptions{ProjectPath: dir, Search: "login"}, 1, "search description")
	count(ListOptions{ProjectPath: dir, Search: "readme"}, 1, "search file")
	count(ListOptions{ProjectPath: dir, Search: "docs"}, 1, "search case-insensitive")
	count(ListOptions{ProjectPath: dir, Search: "nothing here"}, 0, "search miss")

	if _, err := store.ListTasks(ListOptions{ProjectPath: dir, Status: "todo,done"}); !IsValidationError(err) {
		t.Errorf("multiple statuses rejected: err = %v, want ValidationError", err)
	}
	if _, err := store.ListTasks(ListOptions{ProjectPath: dir, Priority: "low,urgent"}); !IsValidationError(err) {
		t.Errorf("multiple priorities rejected: err = %v, want ValidationError", err)
	}
	if _, err := store.ListTasks(ListOptions{ProjectPath: dir, Status: "bogus"}); !IsValidationError(err) {
		t.Errorf("invalid status filter: err = %v, want ValidationError", err)
	}
	if _, err := store.ListTasks(ListOptions{ProjectPath: dir, Priority: "bogus"}); !IsValidationError(err) {
		t.Errorf("invalid priority filter: err = %v, want ValidationError", err)
	}
}

func TestListTasksSort(t *testing.T) {
	store := memStore(t)
	dir := t.TempDir()
	create(t, store, map[string]any{"title": "low one", "priority": "low", "status": "todo", "project_path": dir})
	create(t, store, map[string]any{"title": "urgent one", "priority": "urgent", "status": "todo", "project_path": dir})
	create(t, store, map[string]any{"title": "high one", "priority": "high", "status": "todo", "project_path": dir})

	byPriority, err := store.ListTasks(ListOptions{ProjectPath: dir, Sort: "priority"})
	if err != nil {
		t.Fatalf("sort by priority: %v", err)
	}
	if len(byPriority) != 3 || byPriority[0].Priority != "urgent" || byPriority[2].Priority != "low" {
		t.Errorf("priority sort = %v, want urgent first", titles(byPriority))
	}

	create(t, store, map[string]any{"title": "alpha", "priority": "medium", "status": "todo", "project_path": dir})
	byTitle, err := store.ListTasks(ListOptions{ProjectPath: dir, Sort: "title"})
	if err != nil {
		t.Fatalf("sort by title: %v", err)
	}
	if byTitle[0].Title != "alpha" {
		t.Errorf("title sort first = %q, want alpha", byTitle[0].Title)
	}

	byCreated, err := store.ListTasks(ListOptions{ProjectPath: dir, Sort: "created"})
	if err != nil {
		t.Fatalf("sort by created: %v", err)
	}
	if len(byCreated) != 4 {
		t.Fatalf("created sort returned %d tasks, want 4", len(byCreated))
	}

	// Unknown sort keys fall back to `updated` rather than erroring.
	if _, err := store.ListTasks(ListOptions{ProjectPath: dir, Sort: "nonsense"}); err != nil {
		t.Errorf("unknown sort should fall back, got %v", err)
	}
}

func TestListTasksLimit(t *testing.T) {
	store := memStore(t)
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		create(t, store, map[string]any{"title": fmt.Sprintf("task %d", i), "project_path": dir})
	}

	limited, err := store.ListTasks(ListOptions{ProjectPath: dir, Limit: 2})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d tasks", len(limited))
	}

	defaulted, err := store.ListTasks(ListOptions{ProjectPath: dir})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(defaulted) != 5 {
		t.Errorf("default limit returned %d tasks, want 5", len(defaulted))
	}

	unlimited, err := store.ListTasks(ListOptions{ProjectPath: dir, Limit: -1})
	if err != nil {
		t.Fatalf("ListTasks(unlimited): %v", err)
	}
	if len(unlimited) != 5 {
		t.Errorf("unlimited returned %d tasks, want 5", len(unlimited))
	}
}

// TestListTasksOffsetPagesThroughTheResultSet walks a result set with a
// limit and an offset and checks that every task is handed out exactly once.
func TestListTasksOffsetPagesThroughTheResultSet(t *testing.T) {
	store := memStore(t)
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		create(t, store, map[string]any{"title": fmt.Sprintf("task %d", i), "project_path": dir})
	}

	var (
		seen  []string
		limit = 2
	)
	for offset := 0; ; offset += limit {
		page, err := store.ListTasks(ListOptions{ProjectPath: dir, Limit: limit, Offset: offset})
		if err != nil {
			t.Fatalf("ListTasks(offset %d): %v", offset, err)
		}
		for _, task := range page {
			seen = append(seen, task.ID)
		}
		if len(page) < limit {
			break
		}
	}

	if len(seen) != 5 {
		t.Fatalf("paged through %d tasks, want 5", len(seen))
	}
	unique := make(map[string]bool, len(seen))
	for _, id := range seen {
		if unique[id] {
			t.Errorf("task %s was returned twice", id)
		}
		unique[id] = true
	}

	beyond, err := store.ListTasks(ListOptions{ProjectPath: dir, Limit: 2, Offset: 99})
	if err != nil {
		t.Fatalf("ListTasks(offset past the end): %v", err)
	}
	if len(beyond) != 0 {
		t.Errorf("offset past the end returned %d tasks, want 0", len(beyond))
	}
}

// TestListTasksOrdersEqualTimestampsByID pins the tie-breaker: tasks that
// share a timestamp keep a stable order, so a paged result set never shows
// the same task twice or skips one.
func TestListTasksOrdersEqualTimestampsByID(t *testing.T) {
	store := memStore(t)
	dir := t.TempDir()

	var ids []string
	for i := 0; i < 5; i++ {
		task := create(t, store, map[string]any{"title": fmt.Sprintf("task %d", i), "project_path": dir})
		ids = append(ids, task.ID)
	}
	// Flatten the timestamps so that only the id can break the tie.
	if _, err := store.db.Exec(`UPDATE tasks SET created_at = 1000, updated_at = 1000`); err != nil {
		t.Fatalf("flatten timestamps: %v", err)
	}

	for _, sortName := range []string{"updated", "created", "priority", "title"} {
		tasks, err := store.ListTasks(ListOptions{ProjectPath: dir, Sort: sortName})
		if err != nil {
			t.Fatalf("ListTasks(sort %s): %v", sortName, err)
		}
		got := make([]string, 0, len(tasks))
		for _, task := range tasks {
			got = append(got, task.ID)
		}
		// Every sort breaks the tie on the id here, and the titles run in
		// creation order, so the id order is the expected order throughout.
		if !reflect.DeepEqual(got, ids) {
			t.Errorf("sort %s returned %v, want %v", sortName, got, ids)
		}
	}
}

func TestListTasksReturnsEmptySliceNotNil(t *testing.T) {
	store := memStore(t)
	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if tasks == nil {
		t.Fatal("ListTasks should return an empty slice, not nil, so JSON marshals as []")
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("encoded = %s, want []", encoded)
	}
}

// TestEmptyCollectionsMarshalAsJSONArray guards the SPA contract: the front end
// branches on array-ness (tasks.map / projects.forEach) and breaks on null.
func TestEmptyCollectionsMarshalAsJSONArray(t *testing.T) {
	store := memStore(t)

	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("ListTasks on empty db marshalled %s, want []", encoded)
	}
	if tasks == nil {
		t.Error("ListTasks returned a nil slice; a nil slice marshals to null")
	}

	projects, err := store.GetProjects()
	if err != nil {
		t.Fatalf("GetProjects: %v", err)
	}
	encoded, err = json.Marshal(projects)
	if err != nil {
		t.Fatalf("marshal projects: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("GetProjects on empty db marshalled %s, want []", encoded)
	}
	if projects == nil {
		t.Error("GetProjects returned a nil slice; a nil slice marshals to null")
	}

	// Filters that match nothing must behave identically to an empty database.
	filtered, err := store.ListTasks(ListOptions{All: true, Search: "no-such-task", Status: "done"})
	if err != nil {
		t.Fatalf("ListTasks(filtered): %v", err)
	}
	encoded, err = json.Marshal(filtered)
	if err != nil {
		t.Fatalf("marshal filtered: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("no-match filter marshalled %s, want []", encoded)
	}
}

// TestOptionalFieldsMarshalAsExplicitNull locks in the "null preserved" rule:
// optional columns are always emitted as JSON null rather than being omitted.
func TestOptionalFieldsMarshalAsExplicitNull(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Sparse task"})

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	nullable := []string{
		"description", "file_path", "code_snippet", "agent_notes",
		"line_start", "line_end", "closed_at",
	}
	for _, key := range nullable {
		raw, ok := got[key]
		if !ok {
			t.Errorf("optional field %q was omitted; SPA expects an explicit null", key)
			continue
		}
		if string(raw) != "null" {
			t.Errorf("optional field %q = %s, want null", key, raw)
		}
	}

	// The camelCase aliases must NOT be present.
	for _, key := range []string{"filePath", "codeSnippet", "agentNotes", "lineStart", "lineEnd", "closedAt"} {
		if _, ok := got[key]; ok {
			t.Errorf("alias %q should not be present", key)
		}
	}

	// Non-null round trip must not regress into null.
	populated := create(t, store, map[string]any{
		"title":       "Populated",
		"description": "d",
		"line_start":  float64(5),
	})
	encoded, err = json.Marshal(populated)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["description"]) != `"d"` || string(got["line_start"]) != "5" {
		t.Errorf("populated fields = %s/%s, want \"d\"/5", got["description"], got["line_start"])
	}
}

func TestMixedAliasInputPrefersSnakeCaseEndToEnd(t *testing.T) {
	// End-to-end companion to TestCanonicalizeAliasPrecedence: the winner must
	// survive the whole write path, not just the merge helper.
	for i := 0; i < 25; i++ {
		store := memStore(t)
		task := create(t, store, map[string]any{
			"title":        "snake wins",
			"file_path":    "snake.go",
			"filePath":     "camel.go",
			"agent_notes":  "snake note",
			"agentNotes":   "camel note",
			"line_start":   float64(1),
			"lineStart":    float64(999),
			"project_name": "snakeproject",
			"projectName":  "camelproject",
		})

		if task.FilePath == nil || *task.FilePath != "snake.go" {
			t.Fatalf("iteration %d: FilePath = %v, want snake.go", i, task.FilePath)
		}
		if task.AgentNotes == nil || *task.AgentNotes != "snake note" {
			t.Fatalf("iteration %d: AgentNotes = %v, want snake note", i, task.AgentNotes)
		}
		if task.LineStart == nil || *task.LineStart != 1 {
			t.Fatalf("iteration %d: LineStart = %v, want 1", i, task.LineStart)
		}
		if task.ProjectName != "snakeproject" {
			t.Fatalf("iteration %d: ProjectName = %q, want snakeproject", i, task.ProjectName)
		}
		store.Close()
	}
}

func TestCamelCaseAppliesWhenSnakeCaseAbsent(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{
		"title":       "camel only",
		"filePath":    "camel.go",
		"agentNotes":  "camel note",
		"lineStart":   float64(7),
		"lineEnd":     float64(9),
		"projectPath": t.TempDir(),
		"projectName": "camelproject",
	})
	if task.FilePath == nil || *task.FilePath != "camel.go" {
		t.Errorf("FilePath = %v, want camel.go", task.FilePath)
	}
	if task.AgentNotes == nil || *task.AgentNotes != "camel note" {
		t.Errorf("AgentNotes = %v, want camel note", task.AgentNotes)
	}
	if task.LineStart == nil || *task.LineStart != 7 || task.LineEnd == nil || *task.LineEnd != 9 {
		t.Errorf("line range = %v..%v, want 7..9", task.LineStart, task.LineEnd)
	}
	if task.ProjectName != "camelproject" {
		t.Errorf("ProjectName = %q, want camelproject", task.ProjectName)
	}
}

func TestCanonicalizeAliasPrecedence(t *testing.T) {
	input := map[string]any{
		"file_path":    "snake.go",
		"filePath":     "camel.go",
		"agent_notes":  "snake note",
		"agentNotes":   "camel note",
		"line_start":   float64(1),
		"lineStart":    float64(999),
		"project_name": "snakeproject",
		"projectName":  "camelproject",
		"id":           "ignored",
		"unknown":      "dropped",
	}

	want := map[string]any{
		"file_path":    "snake.go",
		"agent_notes":  "snake note",
		"line_start":   float64(1),
		"project_name": "snakeproject",
	}

	// Go randomises map iteration, so repeat: a single-pass merge would flip
	// the winner between runs.
	for i := 0; i < 500; i++ {
		if got := canonicalize(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: canonicalize = %#v, want %#v", i, got, want)
		}
	}

	// camelCase still applies when snake_case is absent.
	camelOnly := canonicalize(map[string]any{"filePath": "camel.go", "lineStart": float64(3)})
	if camelOnly["file_path"] != "camel.go" || camelOnly["line_start"] != float64(3) {
		t.Errorf("camel-only = %#v", camelOnly)
	}
}

func TestCreateIgnoresCallerSuppliedID(t *testing.T) {
	store := memStore(t)

	task := create(t, store, map[string]any{"title": "No id please", "id": "T202001010001"})
	if task.ID == "T202001010001" {
		t.Fatal("CreateTask must ignore a caller-supplied id")
	}
	if !taskIDPattern.MatchString(task.ID) {
		t.Errorf("ID = %q, want a generated T<YYYYMMDD><NNNN> id", task.ID)
	}
}

func TestUpdateIgnoresCallerSuppliedID(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "Keep my id"})

	updated, err := store.UpdateTask(task.ID, map[string]any{"title": "Renamed", "id": "task_hijack"})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.ID != task.ID {
		t.Errorf("ID = %q, want unchanged %q", updated.ID, task.ID)
	}
	if updated.Title != "Renamed" {
		t.Errorf("Title = %q, want Renamed", updated.Title)
	}
	if _, err := store.UpdateTask(task.ID, map[string]any{"id": "task_hijack"}); !IsValidationError(err) {
		t.Errorf("id-only update: err = %v, want ValidationError", err)
	}
	if _, err := store.GetTask("task_hijack"); err != nil {
		t.Errorf("GetTask: %v", err)
	} else if hijacked, _ := store.GetTask("task_hijack"); hijacked != nil {
		t.Error("an id-only update must not create or move a task")
	}
}

func TestLineNumberOverflowIsRejected(t *testing.T) {
	store := memStore(t)

	// Values at and beyond 2^63 overflow int64, and 2^53+1 is already not
	// representable as a float64. All must be refused, not wrapped or rounded.
	overflow := []any{
		float64(math.MaxInt64), // 9223372036854775807
		math.Exp2(63),          // 2^63
		float64(1<<53) + 2,     // first value that rounds in float64
		uint64(math.MaxUint64), //
		"9223372036854775808",  //
		"99999999999999999999", // beyond uint64
		json.Number("1e30"),    //
		json.Number("9223372036854775808"),
	}
	for _, value := range overflow {
		if _, err := store.CreateTask(map[string]any{"title": "overflow", "line_start": value}); !IsValidationError(err) {
			t.Errorf("line_start %v (%T): err = %v, want ValidationError", value, value, err)
		}
	}

	// The documented ceiling itself must still be accepted.
	max := create(t, store, map[string]any{"title": "max", "line_start": float64(MaxLineNumber)})
	if max.LineStart == nil || *max.LineStart != MaxLineNumber {
		t.Errorf("LineStart = %v, want %d", max.LineStart, MaxLineNumber)
	}
}

func TestLineNumberNativeInt64PreservedExactly(t *testing.T) {
	store := memStore(t)
	// Beyond MaxLineNumber a native int64 keeps its exact value and is refused
	// by the range check rather than being mangled by a float64 round trip.
	if _, err := store.CreateTask(map[string]any{"title": "exact", "line_start": int64(1) << 62}); !IsValidationError(err) {
		t.Errorf("int64 2^62: err = %v, want ValidationError", err)
	}
	task := create(t, store, map[string]any{"title": "exact ok", "line_start": int64(1234567890123)})
	if task.LineStart == nil || *task.LineStart != 1234567890123 {
		t.Errorf("LineStart = %v, want 1234567890123 exactly", task.LineStart)
	}
}

func TestTextFieldsRejectCompositeAndBoolTypes(t *testing.T) {
	store := memStore(t)

	bad := []any{true, false, map[string]any{"a": 1}, []string{"x"}, []any{1, 2}, struct{ A int }{A: 1}}
	for _, value := range bad {
		if _, err := store.CreateTask(map[string]any{"title": value}); !IsValidationError(err) {
			t.Errorf("title %v (%T): err = %v, want ValidationError", value, value, err)
		}
		_, err := store.CreateTask(map[string]any{"title": "ok", "project_path": value})
		if !IsValidationError(err) {
			t.Errorf("project_path %v (%T): err = %v, want ValidationError", value, value, err)
		}
		_, err = store.CreateTask(map[string]any{"title": "ok", "project_name": value})
		if !IsValidationError(err) {
			t.Errorf("project_name %v (%T): err = %v, want ValidationError", value, value, err)
		}
	}

	// Strings and explicit null keep working.
	blank := create(t, store, map[string]any{"title": "blank", "project_name": "   "})
	if blank.ProjectName != filepath.Base(blank.ProjectPath) {
		t.Errorf("ProjectName = %q, want it derived from the path", blank.ProjectName)
	}
	if _, err := store.CreateTask(map[string]any{"title": nil}); !IsValidationError(err) {
		t.Error("null title should be a validation error")
	}
}

func TestUpdateRejectsCompositeTextFields(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{"title": "strict update"})

	for _, fields := range []map[string]any{
		{"title": true},
		{"title": map[string]any{"x": 1}},
		{"project_path": []any{1}},
		{"project_name": map[string]any{"y": 2}},
	} {
		if _, err := store.UpdateTask(task.ID, fields); !IsValidationError(err) {
			t.Errorf("UpdateTask(%v): err = %v, want ValidationError", fields, err)
		}
	}

	unchanged, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if unchanged.Title != "strict update" {
		t.Errorf("Title = %q, want it unchanged after rejected updates", unchanged.Title)
	}
}

func TestStatusAndPriorityStillAcceptNumbers(t *testing.T) {
	store := memStore(t)
	// Enums stay permissive: a numeric status is reported, but the field is
	// still coerced to text rather than rejected as a type error.
	_, err := store.CreateTask(map[string]any{"title": "numeric status", "status": float64(7)})
	if !IsValidationError(err) {
		t.Errorf("numeric status: err = %v, want ValidationError", err)
	}
	task := create(t, store, map[string]any{"title": "ok", "priority": "HIGH"})
	if task.Priority != "high" {
		t.Errorf("Priority = %q, want high (case folded)", task.Priority)
	}
}

func TestGetProjects(t *testing.T) {
	store := memStore(t)
	alphaPath := t.TempDir()
	betaPath := t.TempDir()

	create(t, store, map[string]any{"title": "A1", "project_path": alphaPath, "project_name": "alpha"})
	create(t, store, map[string]any{"title": "A2", "project_path": alphaPath, "project_name": "alpha"})
	create(t, store, map[string]any{"title": "B1", "project_path": betaPath, "project_name": "beta", "status": "done"})

	projects, err := store.GetProjects()
	if err != nil {
		t.Fatalf("GetProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	// Ordered by open_tasks DESC, so alpha (2 open) precedes beta (0 open).
	alpha := projects[0]
	if alpha.Path != alphaPath || alpha.Name != "alpha" {
		t.Errorf("first project = %+v, want alpha at %s", alpha, alphaPath)
	}
	if alpha.TotalTasks != 2 || alpha.OpenTasks != 2 {
		t.Errorf("alpha counts = %+v, want total 2 open 2", alpha)
	}
	if alpha.UpdatedAt <= 0 {
		t.Errorf("alpha.UpdatedAt = %d, want > 0", alpha.UpdatedAt)
	}
	beta := projects[1]
	if beta.Path != betaPath || beta.Name != "beta" {
		t.Errorf("second project = %+v, want beta at %s", beta, betaPath)
	}
	if beta.OpenTasks != 0 {
		t.Errorf("beta.OpenTasks = %d, want 0", beta.OpenTasks)
	}
}

func TestGetStats(t *testing.T) {
	store := memStore(t)
	primary := t.TempDir()
	other := t.TempDir()

	create(t, store, map[string]any{"title": "1", "status": "backlog", "project_path": primary})
	create(t, store, map[string]any{"title": "2", "status": "todo", "project_path": primary})
	create(t, store, map[string]any{"title": "3", "status": "todo", "project_path": primary})
	create(t, store, map[string]any{"title": "4", "status": "in_progress", "project_path": primary})
	create(t, store, map[string]any{"title": "5", "status": "done", "project_path": primary})
	create(t, store, map[string]any{"title": "6", "status": "todo", "project_path": other})

	scoped, err := store.GetStats(primary)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if scoped.Total != 5 || scoped.Backlog != 1 || scoped.Todo != 2 || scoped.InProgress != 1 || scoped.Done != 1 || scoped.Open != 4 {
		t.Errorf("scoped stats = %+v", scoped)
	}
	if scoped.ProjectPath == nil || *scoped.ProjectPath != primary {
		t.Errorf("ProjectPath = %v, want %s", scoped.ProjectPath, primary)
	}

	all, err := store.GetStats("")
	if err != nil {
		t.Fatalf("GetStats(all): %v", err)
	}
	if all.Total != 6 || all.Projects != 2 {
		t.Errorf("global stats = %+v, want total 6 across 2 projects", all)
	}
	if all.ProjectPath != nil {
		t.Errorf("global ProjectPath = %v, want nil", all.ProjectPath)
	}
}

func TestGetStatsOnEmptyDatabase(t *testing.T) {
	store := memStore(t)
	stats, err := store.GetStats("")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Total != 0 || stats.Open != 0 {
		t.Errorf("stats on empty db = %+v, want zeros", stats)
	}
}

// ------------------------------------------------------------------- JSON --

func TestTaskMarshalJSONShape(t *testing.T) {
	store := memStore(t)
	task := create(t, store, map[string]any{
		"title":       "Serialize me",
		"description": "desc",
		"file_path":   "a.go",
		"line_start":  float64(2),
		"line_end":    float64(8),
		"agent_notes": "notes",
		"status":      "done",
	})

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// snake_case fields
	for _, key := range []string{"id", "project_path", "project_name", "title", "status", "priority",
		"description", "file_path", "code_snippet", "agent_notes",
		"line_start", "line_end", "created_at", "updated_at", "closed_at"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing snake_case key %q in %s", key, encoded)
		}
	}
	// camelCase aliases should NOT be present
	for _, key := range []string{"projectPath", "projectName", "filePath", "lineStart", "lineEnd",
		"codeSnippet", "agentNotes", "createdAt", "updatedAt", "closedAt"} {
		if _, ok := got[key]; ok {
			t.Errorf("unexpected camelCase alias %q in %s", key, encoded)
		}
	}

	if got["closed_at"] == nil {
		t.Error("closed_at should be set for a done task")
	}

	// code_snippet was never provided: must be present as null.
	if value, ok := got["code_snippet"]; !ok || value != nil {
		t.Errorf("code_snippet = %v (present=%v), want explicit null", value, ok)
	}
}

func TestProjectAndStatsJSONShape(t *testing.T) {
	project, err := json.Marshal(Project{Path: "/p", Name: "n", TotalTasks: 2, OpenTasks: 1, UpdatedAt: 99})
	if err != nil {
		t.Fatalf("marshal project: %v", err)
	}
	want := `{"path":"/p","name":"n","total_tasks":2,"open_tasks":1,"updated_at":99}`
	if string(project) != want {
		t.Errorf("project JSON = %s, want %s", project, want)
	}

	stats, err := json.Marshal(Stats{Total: 3, Done: 1, Open: 2, Projects: 1})
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	if string(stats) != `{"total":3,"backlog":0,"todo":0,"in_progress":0,"done":1,"open":2,"projects":1,"project_path":null}` {
		t.Errorf("stats JSON = %s", stats)
	}
}

// ------------------------------------------------------------ persistence --

func TestPersistenceAcrossReopen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nested", "tasks.db")

	first := mustOpen(t, file)
	created := create(t, first, map[string]any{
		"title":  "Durable",
		"status": "in_progress",
	})
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := mustOpen(t, file)
	reloaded, err := second.GetTask(created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if reloaded == nil {
		t.Fatal("task did not survive reopen")
	}
	if reloaded.Title != "Durable" || reloaded.Status != "in_progress" {
		t.Errorf("reloaded = %+v", reloaded)
	}
	if reloaded.CreatedAt != created.CreatedAt {
		t.Errorf("CreatedAt changed: %d -> %d", created.CreatedAt, reloaded.CreatedAt)
	}
}

func TestConcurrentCreatesFromMultipleStores(t *testing.T) {
	file := filepath.Join(t.TempDir(), "concurrent.db")

	const stores = 4
	const perStore = 10

	handles := make([]*Store, stores)
	for i := range handles {
		handles[i] = mustOpen(t, file)
	}

	done := make(chan error, stores)
	for i, store := range handles {
		go func(index int, s *Store) {
			for j := 0; j < perStore; j++ {
				_, err := s.CreateTask(map[string]any{"title": fmt.Sprintf("s%d-t%d", index, j)})
				if err != nil {
					done <- fmt.Errorf("store %d create %d: %w", index, j, err)
					return
				}
			}
			done <- nil
		}(i, store)
	}
	for i := 0; i < stores; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	list, err := handles[0].ListTasks(ListOptions{All: true, Limit: -1})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != stores*perStore {
		t.Errorf("got %d tasks, want %d", len(list), stores*perStore)
	}

	// Unique IDs despite concurrent generation.
	ids := map[string]bool{}
	for _, task := range list {
		if ids[task.ID] {
			t.Fatalf("duplicate id generated: %s", task.ID)
		}
		ids[task.ID] = true
	}
}

func TestProjectPathClauseFoldsCase(t *testing.T) {
	// Windows volumes are case-insensitive, so the clause folds case.
	got := projectPathClause("project_path")
	if got != "LOWER(project_path) = LOWER(?)" {
		t.Errorf("clause = %q, want case-insensitive comparison", got)
	}

	// The rule keys off the target column, not a hardcoded column name.
	if other := projectPathClause("id"); other == got {
		t.Errorf("projectPathClause ignored its column argument: %q", other)
	}
}

func TestProjectPathComparisonFoldsCase(t *testing.T) {
	store := memStore(t)
	dir := t.TempDir()
	create(t, store, map[string]any{"title": "case", "project_path": dir})

	upper := strings.ToUpper(dir)
	tasks, err := store.ListTasks(ListOptions{ProjectPath: upper})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("case-different path should match: got %d tasks", len(tasks))
	}

	// GetStats must agree with ListTasks about the same path.
	stats, err := store.GetStats(upper)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Total != 1 {
		t.Errorf("GetStats total = %d, want 1", stats.Total)
	}
}

// -------------------------------------------------------- legacy Node db --
//
// testdata/legacy_node.db was written by the original src/db.js implementation.
// These tests prove the Go store adopts such a file in place: identical schema,
// identical millisecond timestamps, NULLs preserved, and no rebuild.

// legacyWindowsProjectPath is the project_path stored inside the fixture. It is
// a Windows absolute path anonymized for public distribution, so it
// must only ever be compared as an opaque string - never fed to filepath.Abs or
// filepath.IsAbs, which would re-root or reject it on other hosts.
const legacyWindowsProjectPath = `C:\dev\TaskView`

// legacyFixturePath returns the durable Node-created fixture. The path is
// canonical and deliberate: a missing fixture is a hard failure, never a skip,
// so a green run always means the legacy compatibility path was exercised.
// Keep this file unmutated - every test copies it to a temp dir first.
func legacyFixturePath(t *testing.T) string {
	t.Helper()

	const path = "testdata/legacy_node.db"
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("legacy fixture %s is required but unreadable: %v", path, err)
	}
	if info.IsDir() || info.Size() == 0 {
		t.Fatalf("legacy fixture %s is empty or not a file (size %d)", path, info.Size())
	}
	return path
}

// copyFileToTemp keeps the supplied fixture pristine: WAL mode and any future
// writes land on a scratch copy instead. Copying a single file also means WAL
// sidecars left by the Node writer are intentionally not carried over.
func copyFileToTemp(t *testing.T, src string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "legacy.db")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return dst
}

type legacyRow struct {
	ID          string
	ProjectPath string
	ProjectName string
	Title       string
	Status      string
	Priority    string
	Description *string
	FilePath    *string
	CodeSnippet *string
	AgentNotes  *string
	LineStart   *int64
	LineEnd     *int64
	CreatedAt   int64
	UpdatedAt   int64
	ClosedAt    *int64
}

func TestLegacyNodeDatabaseIsAdoptedVerbatim(t *testing.T) {
	src := legacyFixturePath(t)
	path := copyFileToTemp(t, src)

	store := mustOpen(t, path)

	// The Node schema must already satisfy us: apart from the retired `tags`
	// column, which the migration drops, no rebuild happens.
	defaults := map[string]string{}
	rows, err := store.db.Query(`SELECT name, dflt_value FROM pragma_table_info('tasks')`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	for rows.Next() {
		var name string
		var dflt sql.NullString
		if err := rows.Scan(&name, &dflt); err != nil {
			rows.Close()
			t.Fatalf("scan column: %v", err)
		}
		defaults[name] = dflt.String
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate columns: %v", err)
	}
	rows.Close()

	for _, column := range []string{"id", "project_path", "project_name", "title", "description",
		"status", "priority", "file_path", "line_start", "line_end", "code_snippet",
		"agent_notes", "created_at", "updated_at", "closed_at"} {
		if _, ok := defaults[column]; !ok {
			t.Errorf("legacy table is missing column %q", column)
		}
	}
	if _, ok := defaults["tags"]; ok {
		t.Error("legacy table still has a tags column after the migration")
	}
	if len(defaults) != 15 {
		t.Errorf("legacy table has %d columns, want 15", len(defaults))
	}
	if defaults["status"] != "'todo'" || defaults["priority"] != "'medium'" {
		t.Errorf("column defaults = status %q priority %q, want 'todo'/'medium'",
			defaults["status"], defaults["priority"])
	}

	first, err := store.GetTask("task_legacy001_abcd")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if first == nil {
		t.Fatal("legacy task not found")
	}

	// Every field written by Node must round-trip unchanged.
	want := legacyRow{
		ID:          "task_legacy001_abcd",
		ProjectPath: legacyWindowsProjectPath,
		ProjectName: "TaskView",
		Title:       "Legacy task from Node",
		Status:      "in_progress",
		Priority:    "urgent",
		Description: strPtr("created by src/db.js"),
		FilePath:    strPtr("src/db.js"),
		CodeSnippet: strPtr("createTask(data) {\n  // legacy\n}"),
		AgentNotes:  strPtr("note one\nnote two"),
		LineStart:   intPtr(238),
		LineEnd:     intPtr(277),
		CreatedAt:   1792377253188,
		UpdatedAt:   1792377253188,
		ClosedAt:    nil,
	}

	if first.ID != want.ID || first.Title != want.Title {
		t.Errorf("identity changed: %+v", first)
	}
	if first.Status != want.Status || first.Priority != want.Priority {
		t.Errorf("status/priority = %s/%s, want %s/%s", first.Status, first.Priority, want.Status, want.Priority)
	}
	if first.ProjectPath != want.ProjectPath || first.ProjectName != want.ProjectName {
		t.Errorf("project = %q/%q, want %q/%q", first.ProjectPath, first.ProjectName, want.ProjectPath, want.ProjectName)
	}
	if !eqStr(first.Description, want.Description) {
		t.Errorf("Description = %v, want %q", first.Description, *want.Description)
	}
	if !eqStr(first.FilePath, want.FilePath) {
		t.Errorf("FilePath = %v, want %q", first.FilePath, *want.FilePath)
	}
	if !eqStr(first.CodeSnippet, want.CodeSnippet) {
		t.Errorf("CodeSnippet = %v, want %q", first.CodeSnippet, *want.CodeSnippet)
	}
	if !eqStr(first.AgentNotes, want.AgentNotes) {
		t.Errorf("AgentNotes = %v, want %q", first.AgentNotes, *want.AgentNotes)
	}
	if !equalIntPtr(first.LineStart, want.LineStart) || !equalIntPtr(first.LineEnd, want.LineEnd) {
		t.Errorf("line range = %v..%v, want 238..277", first.LineStart, first.LineEnd)
	}
	if first.CreatedAt != want.CreatedAt || first.UpdatedAt != want.UpdatedAt {
		t.Errorf("timestamps = %d/%d, want %d/%d (millisecond precision preserved)",
			first.CreatedAt, first.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
	if first.ClosedAt != nil {
		t.Errorf("ClosedAt = %d, want NULL for an in_progress task", *first.ClosedAt)
	}
}

func TestLegacyNullColumnsStayNull(t *testing.T) {
	src := legacyFixturePath(t)
	store := mustOpen(t, copyFileToTemp(t, src))

	task, err := store.GetTask("task_legacy002_wxyz")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task == nil {
		t.Fatal("legacy task not found")
	}
	if task.Description != nil || task.FilePath != nil || task.CodeSnippet != nil ||
		task.AgentNotes != nil || task.LineStart != nil || task.LineEnd != nil {
		t.Errorf("NULL columns did not stay NULL: %+v", task)
	}
	if task.Status != "done" {
		t.Errorf("Status = %q, want done", task.Status)
	}
	if task.ClosedAt == nil || *task.ClosedAt != task.CreatedAt {
		t.Errorf("ClosedAt = %v, want %d", task.ClosedAt, task.CreatedAt)
	}
	if task.CreatedAt != 1792377253190 || task.UpdatedAt != 1792377253190 {
		t.Errorf("timestamps = %d/%d, want 1792377253190", task.CreatedAt, task.UpdatedAt)
	}
}

func TestLegacyDatabaseAggregatesAndUpdates(t *testing.T) {
	src := legacyFixturePath(t)
	store := mustOpen(t, copyFileToTemp(t, src))

	// The fixture's stored project_path is a Windows absolute path. Resolving it
	// through filepath.Abs would re-root it under the host (and mangle it on
	// POSIX), so every aggregate here is queried across all projects instead.
	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("legacy tasks = %d, want 2", len(tasks))
	}

	stats, err := store.GetStats("")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Total != 2 || stats.InProgress != 1 || stats.Done != 1 || stats.Open != 1 {
		t.Errorf("legacy stats = %+v, want total 2 in_progress 1 done 1", stats)
	}
	if stats.ProjectPath != nil {
		t.Errorf("unscoped ProjectPath = %v, want nil", stats.ProjectPath)
	}

	projects, err := store.GetProjects()
	if err != nil {
		t.Fatalf("GetProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "TaskView" || projects[0].TotalTasks != 2 {
		t.Errorf("legacy projects = %+v", projects)
	}

	// Updating a legacy row must preserve untouched fields and maintain closed_at.
	updated, err := store.UpdateTask("task_legacy001_abcd", map[string]any{"status": "done"})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Status != "done" || updated.ClosedAt == nil {
		t.Errorf("update status = %+v, want done with closed_at set", updated)
	}
	if updated.CreatedAt != 1792377253188 {
		t.Errorf("CreatedAt mutated to %d", updated.CreatedAt)
	}
	if !eqStr(updated.AgentNotes, strPtr("note one\nnote two")) || !eqStr(updated.CodeSnippet, strPtr("createTask(data) {\n  // legacy\n}")) {
		t.Error("untouched fields were altered by update")
	}
	if updated.UpdatedAt <= 1792377253188 {
		t.Errorf("UpdatedAt = %d, want it to advance", updated.UpdatedAt)
	}

	// A new task can coexist in the legacy database without disturbing the rows
	// above. It is given a temp-dir project so the project counts below are
	// deterministic: on the machine that generated the fixture the current
	// directory happens to equal the fixture's project_path, which would make
	// this task silently join the legacy project.
	tempProject := t.TempDir()
	fresh, err := store.CreateTask(map[string]any{
		"title":        "Added by Go",
		"project_path": tempProject,
		"project_name": "TempProject",
	})
	if err != nil {
		t.Fatalf("CreateTask on legacy db: %v", err)
	}
	if !taskIDPattern.MatchString(fresh.ID) {
		t.Errorf("new id = %q, want T<YYYYMMDD><NNNN>", fresh.ID)
	}
	if fresh.Status != "todo" || fresh.Priority != "medium" {
		t.Errorf("defaults = %s/%s, want todo/medium", fresh.Status, fresh.Priority)
	}
	if fresh.ProjectPath != filepath.Clean(tempProject) {
		t.Errorf("new task ProjectPath = %q, want %q", fresh.ProjectPath, filepath.Clean(tempProject))
	}

	// The stored Windows path must survive verbatim on every host, including
	// POSIX ones where it is not a native absolute path.
	reloaded, err := store.GetTask("task_legacy001_abcd")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if reloaded.ProjectPath != legacyWindowsProjectPath {
		t.Errorf("ProjectPath = %q, want it preserved verbatim as %q",
			reloaded.ProjectPath, legacyWindowsProjectPath)
	}

	// Three tasks now span two projects, and the legacy pair is unchanged.
	after, err := store.GetStats("")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if after.Total != 3 {
		t.Errorf("total after insert = %d, want 3", after.Total)
	}
	if after.Projects != 2 {
		t.Errorf("projects after insert = %d, want 2", after.Projects)
	}
	// legacy001 was moved to done above and legacy002 was already done, so the
	// only remaining open task is the new one.
	if after.Done != 2 || after.Todo != 1 || after.InProgress != 0 || after.Open != 1 {
		t.Errorf("status buckets after insert = %+v, want done 2 todo 1 in_progress 0 open 1", after)
	}

	// Scoping to the temp project finds exactly the new task.
	scoped, err := store.GetStats(tempProject)
	if err != nil {
		t.Fatalf("GetStats(temp): %v", err)
	}
	if scoped.Total != 1 || scoped.ProjectPath == nil || filepath.Clean(*scoped.ProjectPath) != filepath.Clean(tempProject) {
		t.Errorf("temp project stats = %+v, want a single task scoped to the temp dir", scoped)
	}
}

// ------------------------------------------------------- tags migration --
//
// The `tags` column was retired from the data model. A database written by an
// older TaskView still carries it, so the store rebuilds the table without it
// on open. The schema below is written out inline - the pre-migration shape -
// so the test does not depend on the repository history.

// legacyTagsSchema is the tasks table (and its indexes) as it looked before
// tags were retired.
const legacyTagsSchema = `
CREATE TABLE tasks (
  id            TEXT PRIMARY KEY,
  project_path  TEXT NOT NULL,
  project_name  TEXT NOT NULL,
  title         TEXT NOT NULL,
  description   TEXT,
  status        TEXT DEFAULT 'todo',
  priority      TEXT DEFAULT 'medium',
  file_path     TEXT,
  line_start    INTEGER,
  line_end      INTEGER,
  code_snippet  TEXT,
  agent_notes   TEXT,
  tags          TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  closed_at     INTEGER
);

CREATE INDEX idx_tasks_project_path ON tasks(project_path);
CREATE INDEX idx_tasks_status        ON tasks(status);
CREATE INDEX idx_tasks_created_at    ON tasks(created_at);
CREATE INDEX idx_tasks_updated_at    ON tasks(updated_at);
CREATE INDEX idx_tasks_priority      ON tasks(priority);
`

// legacyTaskColumns is the pre-migration column list, `tags` included.
const legacyTaskColumns = `id, project_path, project_name, title, description, status, priority,
	file_path, line_start, line_end, code_snippet, agent_notes, tags,
	created_at, updated_at, closed_at`

// legacyTagsRows are the rows the pre-migration database is seeded with. They
// mix NULLs, multi-line text, quotes and non-ASCII characters so a lossy
// rebuild would be visible.
var legacyTagsRows = [][]any{
	{
		"task_legacy_0001", `C:\dev\TaskView`, "TaskView", "Legacy task from Node",
		"created by src/db.js", "in_progress", "urgent", "src/db.js",
		int64(238), int64(277), "createTask(data) {\n  // legacy\n}", "note one\nnote two",
		"backend,legacy", int64(1792377253188), int64(1792377253188), nil,
	},
	{
		"task_legacy_0002", `C:\dev\TaskView`, "TaskView", "标签 «quoted»",
		nil, "done", "low", nil, nil, nil, nil, nil, nil,
		int64(1792377253190), int64(1792377253190), int64(1792377253190),
	},
}

// writeLegacyTagsDB creates a database in the pre-migration shape and seeds it
// with the supplied rows.
func writeLegacyTagsDB(t *testing.T, path string, rows [][]any) {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(legacyTagsSchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO tasks (`+legacyTaskColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row...); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}
}

// dumpTasks reads every row of the tasks table as a column/value map, so two
// dumps can be compared field by field.
func dumpTasks(t *testing.T, db *sql.DB, columns string) []map[string]string {
	t.Helper()

	rows, err := db.Query(`SELECT ` + columns + ` FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatalf("select tasks: %v", err)
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		t.Fatalf("task columns: %v", err)
	}

	var dump []map[string]string
	for rows.Next() {
		cells := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range cells {
			pointers[i] = &cells[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("scan task row: %v", err)
		}
		record := make(map[string]string, len(names))
		for i, cell := range cells {
			record[names[i]] = printableCell(cell)
		}
		dump = append(dump, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate task rows: %v", err)
	}
	return dump
}

// printableCell renders a scanned cell so NULLs and empty strings stay
// distinguishable.
func printableCell(cell any) string {
	switch v := cell.(type) {
	case nil:
		return "<null>"
	case []byte:
		return string(v)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// taskIndexNames returns the names of the indexes owned by the tasks table.
func taskIndexNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND name LIKE 'idx_tasks_%'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}
	return found
}

// TestMigrationDropsTagsColumnAndPreservesEveryOtherField opens a database
// written in the pre-migration shape and checks that the tags column is gone
// while every other value survives the rebuild untouched.
func TestMigrationDropsTagsColumnAndPreservesEveryOtherField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	writeLegacyTagsDB(t, path, legacyTagsRows)

	before := dumpLegacyTasks(t, path)
	if len(before) != len(legacyTagsRows) {
		t.Fatalf("legacy rows = %d, want %d", len(before), len(legacyTagsRows))
	}

	store := mustOpen(t, path)

	has, err := tableHasColumn(store.db, "tasks", "tags")
	if err != nil {
		t.Fatalf("tableHasColumn: %v", err)
	}
	if has {
		t.Error("tasks table still has a tags column after the migration")
	}

	after := dumpTasks(t, store.db, taskColumns)
	if len(after) != len(before) {
		t.Fatalf("row count after migration = %d, want %d", len(after), len(before))
	}
	for i := range before {
		for column, value := range before[i] {
			if column == "tags" {
				continue // deliberately discarded
			}
			got, ok := after[i][column]
			if !ok {
				t.Errorf("row %d lost column %q", i, column)
				continue
			}
			if got != value {
				t.Errorf("row %d column %q = %q after migration, want %q", i, column, got, value)
			}
		}
	}

	// Every index the schema defines must exist on the rebuilt table.
	indexes := taskIndexNames(t, store.db)
	for _, want := range []string{"idx_tasks_project_path", "idx_tasks_status",
		"idx_tasks_created_at", "idx_tasks_updated_at", "idx_tasks_priority"} {
		if !indexes[want] {
			t.Errorf("missing index %s after the migration (found: %v)", want, indexes)
		}
	}

	// The migrated rows are readable through the regular store API.
	tasks, err := store.ListTasks(ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != len(legacyTagsRows) {
		t.Fatalf("listed %d tasks, want %d", len(tasks), len(legacyTagsRows))
	}
	migrated, err := store.GetTask("task_legacy_0001")
	if err != nil || migrated == nil {
		t.Fatalf("GetTask after the migration: %v", err)
	}
	if migrated.Title != "Legacy task from Node" || migrated.Status != "in_progress" {
		t.Errorf("migrated task = %+v, want the legacy title and status", migrated)
	}
}

// TestMigrationIsIdempotentOnAnAlreadyMigratedDatabase opens the same database
// repeatedly: the second and third open must be no-ops that lose nothing.
func TestMigrationIsIdempotentOnAnAlreadyMigratedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	writeLegacyTagsDB(t, path, legacyTagsRows)

	reference := dumpLegacyTasks(t, path)

	for open := 1; open <= 3; open++ {
		store := mustOpen(t, path)

		has, err := tableHasColumn(store.db, "tasks", "tags")
		if err != nil {
			t.Fatalf("open %d: tableHasColumn: %v", open, err)
		}
		if has {
			t.Errorf("open %d: tasks table still has a tags column", open)
		}

		got := dumpTasks(t, store.db, taskColumns)
		if len(got) != len(reference) {
			t.Fatalf("open %d: %d rows, want %d", open, len(got), len(reference))
		}
		for i := range reference {
			for column, value := range reference[i] {
				if column == "tags" {
					continue
				}
				if got[i][column] != value {
					t.Errorf("open %d: row %d column %q = %q, want %q",
						open, i, column, got[i][column], value)
				}
			}
		}

		tasks, err := store.ListTasks(ListOptions{All: true})
		if err != nil {
			t.Fatalf("open %d: ListTasks: %v", open, err)
		}
		if len(tasks) != len(legacyTagsRows) {
			t.Errorf("open %d: listed %d tasks, want %d", open, len(tasks), len(legacyTagsRows))
		}

		if err := store.Close(); err != nil {
			t.Fatalf("open %d: close: %v", open, err)
		}
	}
}

// TestSearchWorksAfterTheTagsMigration proves the search path no longer looks
// at tags: the retired tag value is not searchable, everything else is.
func TestSearchWorksAfterTheTagsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	writeLegacyTagsDB(t, path, legacyTagsRows)

	store := mustOpen(t, path)

	found, err := store.ListTasks(ListOptions{All: true, Search: "node"})
	if err != nil {
		t.Fatalf("search by title: %v", err)
	}
	if len(found) != 1 || found[0].ID != "task_legacy_0001" {
		t.Errorf("search by title = %v, want the legacy task", titles(found))
	}

	found, err = store.ListTasks(ListOptions{All: true, Search: "created by"})
	if err != nil {
		t.Fatalf("search by description: %v", err)
	}
	if len(found) != 1 || found[0].ID != "task_legacy_0001" {
		t.Errorf("search by description = %v, want the legacy task", titles(found))
	}

	// The old tag value is gone with the column, so it must not match.
	found, err = store.ListTasks(ListOptions{All: true, Search: "backend"})
	if err != nil {
		t.Fatalf("search by former tag: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("search by the discarded tag value = %v, want no matches", titles(found))
	}
}

// dumpLegacyTasks snapshots a pre-migration database: it opens the file
// directly, reads every row and closes the handle again.
func dumpLegacyTasks(t *testing.T, path string) []map[string]string {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	return dumpTasks(t, db, legacyTaskColumns)
}

// ------------------------------------------------------------ path helpers --

// TestNormalizeProjectPathRequiresAnAbsolutePath pins the rule the HTTP API
// relies on: a project has to be named explicitly, because the server's
// working directory is the install directory.
func TestNormalizeProjectPathRequiresAnAbsolutePath(t *testing.T) {
	abs := t.TempDir()
	got, err := NormalizeProjectPath(abs)
	if err != nil {
		t.Fatalf("NormalizeProjectPath(%q): %v", abs, err)
	}
	if got != filepath.Clean(abs) {
		t.Errorf("NormalizeProjectPath(%q) = %q, want %q", abs, got, filepath.Clean(abs))
	}

	for _, bad := range []string{"", "   ", "relative/path", "./sibling", "..\\parent"} {
		if _, err := NormalizeProjectPath(bad); !IsValidationError(err) {
			t.Errorf("NormalizeProjectPath(%q): err = %v, want ValidationError", bad, err)
		}
	}
}

func TestProjectNameFromPath(t *testing.T) {
	nested := filepath.Join(string(filepath.Separator), "dev", "my-app")
	if got := ProjectNameFromPath(nested); got != "my-app" {
		t.Errorf("ProjectNameFromPath(%q) = %q, want my-app", nested, got)
	}
	if got := ProjectNameFromPath("  " + nested + "  "); got != "my-app" {
		t.Errorf("ProjectNameFromPath with padding = %q, want my-app", got)
	}
	if got := ProjectNameFromPath(""); got != "" {
		t.Errorf("ProjectNameFromPath(\"\") = %q, want an empty name", got)
	}

	// A volume root has no directory component of its own.
	if got := ProjectNameFromPath("C:\\"); got != "C:" {
		t.Errorf("ProjectNameFromPath(\"C:\\\\\") = %q, want C:", got)
	}
}

func TestPingReadsTheDatabase(t *testing.T) {
	store := memStore(t)
	if err := store.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	closed := mustOpen(t, filepath.Join(t.TempDir(), "closed.db"))
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := closed.Ping(); err == nil {
		t.Error("Ping on a closed store should fail, the health check must not lie")
	}
}

// ---------------------------------------------------------------- helpers --

func strPtr(v string) *string { return &v }

func intPtr(v int64) *int64 { return &v }

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ptr(v int64) *int64 { return &v }

func equalIntPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func titles(tasks []Task) []string {
	out := make([]string, len(tasks))
	for i, task := range tasks {
		out[i] = task.Title
	}
	return out
}
