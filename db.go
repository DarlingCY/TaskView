// Package main implements TaskView's persistence layer.
//
// It is a direct port of the original Node implementation (src/db.js) onto
// database/sql with the pure-Go modernc.org/sqlite driver, so a single static
// binary replaces the Node runtime (CGO_ENABLED=0).
//
// The SQLite schema, column names and millisecond timestamps are byte-for-byte
// compatible with the Node version: an existing ~/.taskview/tasks.db is opened
// in place, and only the retired `tags` column is dropped (by rebuilding the
// table once, see migrateDropTagsColumn). New tasks get ordered
// "T<YYYYMMDD><NNNN>" IDs, while legacy "task_..." IDs already in the file
// remain valid as-is.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// Valid values for Task.Status and ListOptions.Status.
//
// The order is the lifecycle order: a task is prepared (backlog), committed
// (todo), worked on (in_progress), handed over for verification (review) and
// only then closed (done). The order is documentation only - every status can
// be set from every other one, there is no mandatory approval flow.
var (
	Statuses   = []string{"backlog", "todo", "in_progress", "review", "done"}
	Priorities = []string{"low", "medium", "high", "urgent"}
)

// Store defaults.
const (
	DefaultStatus   = "todo"
	DefaultPriority = "medium"

	// MaxListLimit bounds the page size a listing may ask for, MinListLimit
	// keeps a page from being empty. A page is therefore never empty and
	// never unbounded, so a client can always page through a result set.
	MaxListLimit = 500
	MinListLimit = 1

	// DefaultLimit is the page size used when a caller does not ask for one.
	DefaultLimit = MaxListLimit

	BusyTimeoutMS = 5000

	// MaxLineNumber bounds line_start/line_end. Source files are far below this,
	// and the cap keeps every value exactly representable in the float64 that
	// encoding/json hands us, so no conversion can round or wrap.
	MaxLineNumber = int64(1) << 53

	defaultDataDir = ".taskview"
)

// ValidationError reports user-correctable input problems (bad title, unknown
// status, malformed line numbers, ...). Callers use IsValidationError to map it
// to a CLI exit code or an HTTP 400 instead of a 500.
type ValidationError struct {
	Message string
}

// Error implements the error interface.
func (e *ValidationError) Error() string { return e.Message }

// IsValidationError reports whether err (or anything it wraps) is a
// *ValidationError.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

func newValidationError(format string, args ...any) *ValidationError {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// Store is an open task database. It is safe for concurrent use by multiple
// goroutines; separate Store values may point at the same file and the driver
// coordinates them via WAL plus busy_timeout.
type Store struct {
	FilePath string

	db *sql.DB
}

// Task is a single tracked unit of work.
//
// Pointer fields mirror SQL NULL: they marshal as JSON null rather than being
// omitted, which keeps the SPA's rendering stable. List-valued columns are
// always []string in JSON (empty, never null) for the same reason.
type Task struct {
	ID          string  `json:"id"`
	ProjectPath string  `json:"project_path"`
	ProjectName string  `json:"project_name"`
	Title       string  `json:"title"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`
	Description *string `json:"description"`
	FilePath    *string `json:"file_path"`
	CodeSnippet *string `json:"code_snippet"`
	AgentNotes  *string `json:"agent_notes"`
	LineStart   *int64  `json:"line_start"`
	LineEnd     *int64  `json:"line_end"`
	CreatedAt   int64   `json:"created_at"`
	UpdatedAt   int64   `json:"updated_at"`
	ClosedAt    *int64  `json:"closed_at"`
}

// Project is a per-project aggregate used by the projects view.
type Project struct {
	Path            string `json:"path"`
	Name            string `json:"name"`
	TotalTasks      int64  `json:"total_tasks"`
	OpenTasks       int64  `json:"open_tasks"`
	BacklogTasks    int64  `json:"backlog_tasks"`
	TodoTasks       int64  `json:"todo_tasks"`
	InProgressTasks int64  `json:"in_progress_tasks"`
	ReviewTasks     int64  `json:"review_tasks"`
	UpdatedAt       int64  `json:"updated_at"`
}

// Stats holds status bucket counts, optionally scoped to one project.
type Stats struct {
	Total       int64   `json:"total"`
	Backlog     int64   `json:"backlog"`
	Todo        int64   `json:"todo"`
	InProgress  int64   `json:"in_progress"`
	Review      int64   `json:"review"`
	Done        int64   `json:"done"`
	Open        int64   `json:"open"`
	Projects    int64   `json:"projects"`
	ProjectPath *string `json:"project_path"`
}

// ListOptions controls filtering, sorting and paging in ListTasks.
type ListOptions struct {
	ProjectPath string // empty or "all" means "use the current directory"
	Status      string // comma separated; empty means no status filter
	Priority    string // comma separated; empty means no priority filter
	Search      string // case-insensitive substring over title/description/notes/file
	Sort        string // updated | created | priority | title
	All         bool   // ignore the project filter
	Limit       int    // 0 means DefaultLimit; negative means unlimited
	Offset      int    // rows to skip; negative values are treated as zero
}

// taskTableBody is the column list of the tasks table, in schema order. It is
// the single source of truth for the CREATE TABLE statement and for the
// rebuild the tags migration performs.
const taskTableBody = `
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
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  closed_at     INTEGER
`

// taskIndexes are the indexes owned by the tasks table. The tags migration
// rebuilds the table, which drops them, so the same list is replayed verbatim
// on the new table.
var taskIndexes = []string{
	"CREATE INDEX IF NOT EXISTS idx_tasks_project_path ON tasks(project_path)",
	"CREATE INDEX IF NOT EXISTS idx_tasks_status        ON tasks(status)",
	"CREATE INDEX IF NOT EXISTS idx_tasks_created_at    ON tasks(created_at)",
	"CREATE INDEX IF NOT EXISTS idx_tasks_updated_at    ON tasks(updated_at)",
	"CREATE INDEX IF NOT EXISTS idx_tasks_priority      ON tasks(priority)",
}

// schema creates the tasks table and its indexes. It matches the Node
// implementation's CREATE statements minus the retired `tags` column, so an
// existing database is adopted as-is.
var schema = "CREATE TABLE IF NOT EXISTS tasks (" + taskTableBody + ");\n" +
	strings.Join(taskIndexes, ";\n") + ";"

// orderClauses maps a sort name onto its SQL ORDER BY clause.
//
// Every clause ends with the task id, which is unique. Without it two tasks
// that share a timestamp keep an arbitrary relative order, and a paged result
// set could show the same task twice or skip one entirely.
var orderClauses = map[string]string{
	"updated":  "updated_at DESC, created_at DESC, id ASC",
	"created":  "created_at DESC, id ASC",
	"priority": "CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, updated_at DESC, id ASC",
	"title":    "title COLLATE NOCASE ASC, id ASC",
}

// fieldAliases maps every accepted input key to its column. Both snake_case
// (HTTP/DB) and camelCase (CLI/SPA) spellings are accepted; anything else is
// ignored.
var fieldAliases = map[string]string{
	"title":        "title",
	"description":  "description",
	"status":       "status",
	"priority":     "priority",
	"file_path":    "file_path",
	"filePath":     "file_path",
	"line_start":   "line_start",
	"lineStart":    "line_start",
	"line_end":     "line_end",
	"lineEnd":      "line_end",
	"code_snippet": "code_snippet",
	"codeSnippet":  "code_snippet",
	"agent_notes":  "agent_notes",
	"agentNotes":   "agent_notes",
	"project_path": "project_path",
	"projectPath":  "project_path",
	"project_name": "project_name",
	"projectName":  "project_name",
}

// --------------------------------------------------------------- paths ----

// DataDir returns the directory holding the database, honouring TASKVIEW_HOME
// and falling back to ~/.taskview.
func DataDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("TASKVIEW_HOME")); home != "" {
		return filepath.Clean(home), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, defaultDataDir), nil
}

// DefaultDBPath returns ~/.taskview/tasks.db (or $TASKVIEW_HOME/tasks.db).
func DefaultDBPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tasks.db"), nil
}

// resolveProjectPath makes a project path absolute relative to the working
// directory, mirroring Node's path.resolve().
func resolveProjectPath(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return filepath.Clean(cwd), nil
	}
	if !filepath.IsAbs(trimmed) {
		abs, err := filepath.Abs(trimmed)
		if err != nil {
			return "", fmt.Errorf("resolve project path %q: %w", trimmed, err)
		}
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(trimmed), nil
}

// projectPathClause is the SQL predicate used to scope queries to one project.
//
// Windows filesystems are case-insensitive, so the comparison folds case.
func projectPathClause(column string) string {
	return "LOWER(" + column + ") = LOWER(?)"
}

// NormalizeProjectPath validates a project path the caller has to name
// explicitly and returns its cleaned, absolute form.
//
// Unlike resolveProjectPath it never falls back to the working directory: a
// server's working directory is the directory it was installed in, so an
// implicit scope would silently file tasks under a project the user never
// named. A missing or relative path is therefore a validation error.
func NormalizeProjectPath(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", newValidationError("project_path is required and must be an absolute path")
	}
	if !filepath.IsAbs(trimmed) {
		return "", newValidationError("project_path %q must be an absolute path", trimmed)
	}
	return filepath.Clean(trimmed), nil
}

// ProjectNameFromPath derives a project's display name from its path: the
// last directory component ("C:\dev\my-app" becomes "my-app").
//
// A volume root ("C:\" or "/") has no directory component of its own, so the
// volume name is used instead of the whole path.
func ProjectNameFromPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	base := filepath.Base(cleaned)
	if base == "." || base == string(filepath.Separator) {
		if volume := filepath.VolumeName(cleaned); volume != "" {
			return volume
		}
	}
	return base
}

// --------------------------------------------------------------- store ----

// OpenStore opens (creating if needed) the task database at filename. Pass
// ":memory:" for an ephemeral database.
func OpenStore(filename string) (*Store, error) {
	if strings.TrimSpace(filename) == "" {
		def, err := DefaultDBPath()
		if err != nil {
			return nil, err
		}
		filename = def
	}

	dsn := filename
	resolved := ""
	if filename != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		abs, err := filepath.Abs(filename)
		if err != nil {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		resolved = filepath.Clean(abs)
		dsn = resolved
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", dsn, err)
	}
	// SQLite permits a single writer. Capping the pool at one connection makes
	// concurrent callers queue on the busy timeout rather than trip
	// SQLITE_BUSY. Trade-off: a caller that holds an open rows cursor and then
	// issues another query on the same Store will block, so every method here
	// drains and closes its cursor before returning.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA busy_timeout = " + strconv.Itoa(BusyTimeoutMS),
		"PRAGMA foreign_keys = ON",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	// WAL is best-effort: it is unavailable for :memory: and on some network
	// filesystems, and a rollback journal still works.
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		_ = err // non-fatal
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialise schema: %w", err)
	}
	// A database created before tags were retired still carries the column; the
	// migration rebuilds the table without it. On an already-migrated database
	// this is a cheap no-op.
	if err := migrateDropTagsColumn(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	return &Store{db: db, FilePath: resolved}, nil
}

// migrateDropTagsColumn removes the legacy `tags` column from the tasks
// table.
//
// SQLite cannot drop a column safely across the versions TaskView supports, so
// the table is rebuilt: the rows are copied into a fresh table with the
// current schema and the old one is swapped out. The tag values are discarded
// on purpose - tags are no longer part of the data model.
//
// The migration is idempotent: a database whose tasks table has no `tags`
// column is left completely untouched.
func migrateDropTagsColumn(db *sql.DB) error {
	has, err := tableHasColumn(db, "tasks", "tags")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}

	// BEGIN IMMEDIATE takes the write lock before anything is touched, so a
	// second process opening the same database waits on the busy timeout
	// instead of racing through the rebuild.
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin tags migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec("ROLLBACK")
		}
	}()

	// Re-check under the write lock: another process may have migrated the
	// database between the first probe and this transaction.
	has, err = tableHasColumn(db, "tasks", "tags")
	if err != nil {
		return err
	}
	if has {
		steps := []string{
			"CREATE TABLE tasks_new (" + taskTableBody + ")",
			"INSERT INTO tasks_new (" + taskColumns + ") SELECT " + taskColumns + " FROM tasks",
			"DROP TABLE tasks",
			"ALTER TABLE tasks_new RENAME TO tasks",
		}
		for _, step := range steps {
			if _, err := db.Exec(step); err != nil {
				return fmt.Errorf("migrate tags column: %w", err)
			}
		}
		for _, index := range taskIndexes {
			if _, err := db.Exec(index); err != nil {
				return fmt.Errorf("recreate index after tags migration: %w", err)
			}
		}
	}

	if _, err := db.Exec("COMMIT"); err != nil {
		return fmt.Errorf("commit tags migration: %w", err)
	}
	committed = true
	return nil
}

// tableHasColumn reports whether the named table has the named column.
func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("inspect %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	return false, nil
}

// Close releases the underlying database handle. sql.DB.Close is already
// idempotent, so the handle is left in place and every method keeps returning
// the standard "database is closed" error rather than racing on a nil pointer.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) conn() (*sql.DB, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is closed")
	}
	return s.db, nil
}

// ----------------------------------------------------------- conversions --

// toStringPtr trims and converts a value to *string, treating nil as null.
func toStringPtr(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		return &v, nil
	case *string:
		if v == nil {
			return nil, nil
		}
		if *v == "" {
			return nil, nil
		}
		return v, nil
	case json.Number:
		s := string(v)
		return &s, nil
	default:
		return nil, newValidationError("Invalid value: expected a string, got %T", value)
	}
}

// toIntPtr converts JSON numbers (float64), strings from the CLI, and native
// integer types into *int64. Values must be whole, non-negative and within
// [0, MaxLineNumber]; anything else is a validation error.
//
// The upper bound matters: encoding/json decodes numbers as float64, and every
// integer above 2^53 stops being representable there, so a naive
// int64(float64) conversion would silently round or wrap to a negative line
// number. Native int64 inputs bypass that problem and are preserved exactly.
func toIntPtr(value any, label string) (*int64, error) {
	if value == nil {
		return nil, nil
	}

	var n int64
	switch v := value.(type) {
	case int64:
		n = v
	case int:
		n = int64(v)
	case int32:
		n = int64(v)
	case uint:
		n = int64(v)
	case uint32:
		n = int64(v)
	case uint64:
		if v > uint64(MaxLineNumber) {
			return nil, rangeError(label, value)
		}
		n = int64(v)
	case float64, float32:
		f := asFloat64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
			return nil, integerError(label, value)
		}
		if f > float64(MaxLineNumber) {
			return nil, rangeError(label, value)
		}
		n = int64(f)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			n = parsed
		} else {
			// Tolerate "12.0"/"1e3" style input, but nothing lossy.
			parsedFloat, ferr := v.Float64()
			if ferr != nil || parsedFloat != math.Trunc(parsedFloat) {
				return nil, integerError(label, value)
			}
			n = int64(parsedFloat)
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, nil
		}
		if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			n = parsed
		} else {
			parsedFloat, ferr := strconv.ParseFloat(trimmed, 64)
			if ferr != nil || parsedFloat != math.Trunc(parsedFloat) {
				return nil, integerError(label, value)
			}
			if parsedFloat > float64(MaxLineNumber) {
				return nil, rangeError(label, value)
			}
			n = int64(parsedFloat)
		}
	default:
		return nil, newValidationError("Invalid %s: expected an integer, got %T", label, value)
	}

	if n < 0 {
		return nil, newValidationError("Invalid %s: %v must not be negative", label, value)
	}
	if n > MaxLineNumber {
		return nil, rangeError(label, value)
	}
	return &n, nil
}

func asFloat64(v any) float64 {
	switch f := v.(type) {
	case float64:
		return f
	case float32:
		return float64(f)
	}
	return 0
}

func integerError(label string, value any) error {
	return newValidationError("Invalid %s: %v must be an integer", label, value)
}

func rangeError(label string, value any) error {
	return newValidationError("Invalid %s: %v is out of range (max %d)", label, value, MaxLineNumber)
}

// ParseLineRange parses "12-30", "42" or "7-" into a start/end pair. Either
// result may be nil for open-ended input.
func ParseLineRange(input string) (start, end *int64, err error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil, nil, nil
	}
	if n, e := strconv.ParseInt(raw, 10, 64); e == nil {
		if n < 0 {
			return nil, nil, newValidationError("Invalid line range %q: lines must not be negative", input)
		}
		v := n
		return &v, &v, nil
	}
	idx := strings.Index(raw, "-")
	if idx < 0 {
		return nil, nil, newValidationError("Invalid line range %q. Use format like \"12-30\" or \"12\"", input)
	}
	left := strings.TrimSpace(raw[:idx])
	right := strings.TrimSpace(raw[idx+1:])
	parse := func(s string) (*int64, error) {
		if s == "" {
			return nil, nil
		}
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil || n < 0 {
			return nil, newValidationError("Invalid line range %q: %q must be a non-negative integer", input, s)
		}
		return &n, nil
	}
	if start, err = parse(left); err != nil {
		return nil, nil, err
	}
	if end, err = parse(right); err != nil {
		return nil, nil, err
	}
	return start, end, nil
}

// Task IDs are ordered, per-day codes: "T" + YYYYMMDD + a four-digit sequence
// starting at 0001, e.g. T202610190001. The sequence is derived from the rows
// already stored for that day rather than from a counter table. Known limit:
// deleting the newest task of a day frees its number, so a later task created
// that same day can reuse it. Upgrade to a counter table if IDs must never be
// reused.
const taskIDDateLayout = "20060102"

// taskIDPrefix returns the "T<YYYYMMDD>" prefix for the given time.
func taskIDPrefix(now time.Time) string {
	return "T" + now.Format(taskIDDateLayout)
}

// formatTaskID renders prefix plus a four-digit sequence, e.g. T202610190001.
func formatTaskID(prefix string, seq int) string {
	return fmt.Sprintf("%s%04d", prefix, seq)
}

// nextTaskIDSeq returns the highest sequence already used under prefix, or 0
// when the prefix has no rows yet. An unparsable id is reported rather than
// treated as zero, because restarting at 1 would silently collide with rows the
// database already holds.
func nextTaskIDSeq(db *sql.DB, prefix string) (int, error) {
	var maxID sql.NullString
	err := db.QueryRow(`SELECT MAX(id) FROM tasks WHERE id LIKE ?`, prefix+"%").Scan(&maxID)
	if err != nil {
		return 0, fmt.Errorf("find last task id for %s: %w", prefix, err)
	}
	if !maxID.Valid || maxID.String == "" {
		return 0, nil
	}
	suffix := strings.TrimPrefix(maxID.String, prefix)
	seq, err := strconv.Atoi(suffix)
	if err != nil {
		return 0, fmt.Errorf("parse task id %q for %s: %w", maxID.String, prefix, err)
	}
	return seq, nil
}

// ---------------------------------------------------------------- create --

// CreateTask inserts a task. Unknown keys in data are ignored; snake_case and
// camelCase aliases are both accepted. Missing project information is resolved
// from the current working directory.
func (s *Store) CreateTask(data map[string]any) (*Task, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]any{}
	}

	// Canonicalise aliases so the rest of the function reads one spelling.
	canonical := canonicalize(data)

	rawProjectPath, err := requiredString(canonical, "project_path")
	if err != nil {
		return nil, err
	}
	projectPath, err := resolveProjectPath(rawProjectPath)
	if err != nil {
		return nil, err
	}
	projectName, err := requiredString(canonical, "project_name")
	if err != nil {
		return nil, err
	}
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		projectName = ProjectNameFromPath(projectPath)
	}

	title, err := requiredString(canonical, "title")
	if err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, newValidationError("Task title is required")
	}

	status, err := normalizeStatus(canonical["status"], DefaultStatus)
	if err != nil {
		return nil, err
	}
	priority, err := normalizePriority(canonical["priority"], DefaultPriority)
	if err != nil {
		return nil, err
	}
	lineStart, err := toIntPtr(canonical["line_start"], "line_start")
	if err != nil {
		return nil, err
	}
	lineEnd, err := toIntPtr(canonical["line_end"], "line_end")
	if err != nil {
		return nil, err
	}
	if err := validateLineRange(lineStart, lineEnd); err != nil {
		return nil, err
	}

	description, err := toStringPtr(canonical["description"])
	if err != nil {
		return nil, err
	}
	filePath, err := toStringPtr(canonical["file_path"])
	if err != nil {
		return nil, err
	}
	codeSnippet, err := toStringPtr(canonical["code_snippet"])
	if err != nil {
		return nil, err
	}
	agentNotes, err := toStringPtr(canonical["agent_notes"])
	if err != nil {
		return nil, err
	}

	// IDs are server-assigned; an "id" in the payload is ignored.
	//
	// One clock reading supplies both the daily prefix and the timestamps, so an
	// insert that straddles midnight cannot pair yesterday's prefix with today's
	// created_at.
	now := time.Now()
	prefix := taskIDPrefix(now)
	ts := now.UnixMilli()

	var closedAt any
	if status == "done" {
		closedAt = ts
	}

	for attempt := 0; attempt < 100; attempt++ {
		seq, err := nextTaskIDSeq(db, prefix)
		if err != nil {
			return nil, fmt.Errorf("create task: %w", err)
		}
		if seq+1 > 9999 {
			return nil, fmt.Errorf("daily task id space exhausted for %s: 9999 ids already used",
				now.Format(taskIDDateLayout))
		}
		id := formatTaskID(prefix, seq+1)

		// ON CONFLICT DO NOTHING instead of a plain INSERT: a second process
		// racing for the same number inserts nothing, and the loop re-reads the
		// maximum, which that process has by then advanced.
		res, err := db.Exec(`
			INSERT INTO tasks (
				id, project_path, project_name, title, description, status, priority,
				file_path, line_start, line_end, code_snippet, agent_notes,
				created_at, updated_at, closed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			id, projectPath, projectName, title, description, status, priority,
			filePath, lineStart, lineEnd, codeSnippet, agentNotes,
			ts, ts, closedAt)
		if err != nil {
			return nil, fmt.Errorf("create task: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("create task: %w", err)
		}
		if affected == 1 {
			return s.GetTask(id)
		}
		// Zero rows: another process took that number between our SELECT and
		// INSERT. Try the next one.
	}

	return nil, fmt.Errorf("create task: could not allocate a task id for %s after repeated contention",
		prefix)
}

// ------------------------------------------------------------------- read --

const taskColumns = `id, project_path, project_name, title, description, status, priority,
	file_path, line_start, line_end, code_snippet, agent_notes,
	created_at, updated_at, closed_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(row scanner) (*Task, error) {
	var (
		task                    Task
		description, filePath   sql.NullString
		codeSnippet, agentNotes sql.NullString
		lineStart, lineEnd      sql.NullInt64
		closedAt                sql.NullInt64
	)
	err := row.Scan(
		&task.ID, &task.ProjectPath, &task.ProjectName, &task.Title,
		&description, &task.Status, &task.Priority,
		&filePath, &lineStart, &lineEnd, &codeSnippet, &agentNotes,
		&task.CreatedAt, &task.UpdatedAt, &closedAt,
	)
	if err != nil {
		return nil, err
	}

	task.Description = nullString(description)
	task.FilePath = nullString(filePath)
	task.CodeSnippet = nullString(codeSnippet)
	task.AgentNotes = nullString(agentNotes)
	task.LineStart = nullInt(lineStart)
	task.LineEnd = nullInt(lineEnd)
	task.ClosedAt = nullInt(closedAt)
	return &task, nil
}

func nullString(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func nullInt(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64
	return &v
}

// GetTask returns the task with the given id, or (nil, nil) when it does not
// exist.
func (s *Store) GetTask(id string) (*Task, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task %q: %w", id, err)
	}
	return task, nil
}

// ListTasks returns tasks matching opts, newest-updated first by default.
func (s *Store) ListTasks(opts ListOptions) ([]Task, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	var (
		where []string
		args  []any
	)

	if !opts.All {
		target, err := resolveProjectPath(opts.ProjectPath)
		if err != nil {
			return nil, err
		}
		where = append(where, projectPathClause("project_path"))
		args = append(args, target)
	}

	if raw := strings.TrimSpace(opts.Status); raw != "" {
		if strings.Contains(raw, ",") {
			return nil, newValidationError("status filter only allows a single value")
		}
		st, err := normalizeStatus(raw, "")
		if err != nil {
			return nil, err
		}
		where = append(where, "status = ?")
		args = append(args, st)
	}

	if raw := strings.TrimSpace(opts.Priority); raw != "" {
		if strings.Contains(raw, ",") {
			return nil, newValidationError("priority filter only allows a single value")
		}
		pr, err := normalizePriority(raw, "")
		if err != nil {
			return nil, err
		}
		where = append(where, "priority = ?")
		args = append(args, pr)
	}

	if term := strings.TrimSpace(opts.Search); term != "" {
		where = append(where,
			`(LOWER(title) LIKE ? OR LOWER(IFNULL(description, '')) LIKE ? OR
			  LOWER(IFNULL(agent_notes, '')) LIKE ? OR LOWER(IFNULL(file_path, '')) LIKE ?)`)
		like := "%" + strings.ToLower(term) + "%"
		args = append(args, like, like, like, like)
	}

	order, ok := orderClauses[strings.ToLower(strings.TrimSpace(opts.Sort))]
	if !ok {
		order = orderClauses["updated"]
	}

	query := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY ` + order

	limit := opts.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	} else if offset > 0 {
		// SQLite only accepts OFFSET together with LIMIT; -1 means "no limit".
		query += ` LIMIT -1`
	}
	if offset > 0 {
		query += ` OFFSET ?`
		args = append(args, offset)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	tasks := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return tasks, nil
}

// GetProjects aggregates tasks per project.
func (s *Store) GetProjects() ([]Project, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT project_path AS path,
		       project_name AS name,
		       COUNT(*) AS total_tasks,
		       SUM(CASE WHEN status != 'done' THEN 1 ELSE 0 END) AS open_tasks,
		       SUM(CASE WHEN status = 'backlog' THEN 1 ELSE 0 END) AS backlog_tasks,
		       SUM(CASE WHEN status = 'todo' THEN 1 ELSE 0 END) AS todo_tasks,
		       SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END) AS in_progress_tasks,
		       SUM(CASE WHEN status = 'review' THEN 1 ELSE 0 END) AS review_tasks,
		       MAX(updated_at) AS updated_at
		FROM tasks
		GROUP BY project_path, project_name
		ORDER BY open_tasks DESC, total_tasks DESC, name COLLATE NOCASE ASC`)
	if err != nil {
		return nil, fmt.Errorf("get projects: %w", err)
	}
	defer rows.Close()

	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.Path, &p.Name, &p.TotalTasks, &p.OpenTasks,
			&p.BacklogTasks, &p.TodoTasks, &p.InProgressTasks, &p.ReviewTasks, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("get projects: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get projects: %w", err)
	}
	return projects, nil
}

// ProjectNames lists the distinct project names recorded at path.
func (s *Store) ProjectNames(path string) ([]string, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT DISTINCT project_name
		FROM tasks
		WHERE `+projectPathClause("project_path")+`
		ORDER BY project_name COLLATE NOCASE ASC`, path)
	if err != nil {
		return nil, fmt.Errorf("get project names: %w", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("get project names: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get project names: %w", err)
	}
	return names, nil
}

// GetStats counts tasks per status. Pass an empty projectPath for a global
// summary.
func (s *Store) GetStats(projectPath string) (Stats, error) {
	db, err := s.conn()
	if err != nil {
		return Stats{}, err
	}

	query := `SELECT COUNT(*) AS total,
		SUM(CASE WHEN status = 'backlog' THEN 1 ELSE 0 END) AS backlog,
		SUM(CASE WHEN status = 'todo' THEN 1 ELSE 0 END) AS todo,
		SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END) AS in_progress,
		SUM(CASE WHEN status = 'review' THEN 1 ELSE 0 END) AS review,
		SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END) AS done,
		COUNT(DISTINCT project_path) AS projects
		FROM tasks`
	var args []any
	var scoped *string
	if p := strings.TrimSpace(projectPath); p != "" && p != "all" {
		resolved, err := resolveProjectPath(p)
		if err != nil {
			return Stats{}, err
		}
		scoped = &resolved
		query += ` WHERE ` + projectPathClause("project_path")
		args = append(args, resolved)
	}

	var total, backlog, todo, inProgress, review, done, projects sql.NullInt64
	if err := db.QueryRow(query, args...).Scan(
		&total, &backlog, &todo, &inProgress, &review, &done, &projects,
	); err != nil {
		return Stats{}, fmt.Errorf("get stats: %w", err)
	}

	t := total.Int64
	d := done.Int64
	return Stats{
		Total:       t,
		Backlog:     backlog.Int64,
		Todo:        todo.Int64,
		InProgress:  inProgress.Int64,
		Review:      review.Int64,
		Done:        d,
		Open:        t - d,
		Projects:    projects.Int64,
		ProjectPath: scoped,
	}, nil
}

// ----------------------------------------------------------------- update --

// UpdateTask applies fields to the task with the given id. Unknown keys are
// ignored; passing no recognised field is a validation error. Entering "done"
// sets closed_at; leaving "done" clears it. Other transitions leave it alone.
// Returns (nil, nil) when the task does not exist.
func (s *Store) UpdateTask(id string, fields map[string]any) (*Task, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	existing, err := s.GetTask(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	canonical := canonicalize(fields)

	// Order is irrelevant to SQL, but a stable order keeps logs predictable.
	keys := make([]string, 0, len(canonical))
	for k := range canonical {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var (
		sets []string
		args []any
	)
	rawNextStatus, statusProvided := canonical["status"]
	var nextStatus *string
	if statusProvided {
		normalized, err := normalizeStatus(rawNextStatus, "")
		if err != nil {
			return nil, err
		}
		nextStatus = &normalized
	}

	for _, key := range keys {
		value := canonical[key]
		switch key {
		case "title":
			title, err := requiredString(canonical, "title")
			if err != nil {
				return nil, err
			}
			title = strings.TrimSpace(title)
			if title == "" {
				return nil, newValidationError("Task title is required")
			}
			sets = append(sets, "title = ?")
			args = append(args, title)

		case "status":
			sets = append(sets, "status = ?")
			args = append(args, *nextStatus)

		case "priority":
			priority, err := normalizePriority(value, "")
			if err != nil {
				return nil, err
			}
			sets = append(sets, "priority = ?")
			args = append(args, priority)

		case "project_path":
			raw, err := requiredString(canonical, "project_path")
			if err != nil {
				return nil, err
			}
			resolved, err := resolveProjectPath(raw)
			if err != nil {
				return nil, err
			}
			sets = append(sets, "project_path = ?")
			args = append(args, resolved)

		case "project_name":
			name, err := requiredString(canonical, "project_name")
			if err != nil {
				return nil, err
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, newValidationError("Task project_name is required")
			}
			sets = append(sets, "project_name = ?")
			args = append(args, name)

		case "line_start":
			n, err := toIntPtr(value, "line_start")
			if err != nil {
				return nil, err
			}
			sets = append(sets, "line_start = ?")
			args = append(args, n)

		case "line_end":
			n, err := toIntPtr(value, "line_end")
			if err != nil {
				return nil, err
			}
			sets = append(sets, "line_end = ?")
			args = append(args, n)

		case "description", "file_path", "code_snippet", "agent_notes":
			ptr, err := toStringPtr(value)
			if err != nil {
				return nil, err
			}
			sets = append(sets, key+" = ?")
			args = append(args, ptr)
		}
		// Canonicalization already dropped unknown keys.
	}

	if len(sets) == 0 {
		return nil, newValidationError("No valid fields provided to update")
	}

	// Line ranges must stay coherent even when only one bound is supplied.
	newStart, newEnd := existing.LineStart, existing.LineEnd
	for _, key := range keys {
		if key != "line_start" && key != "line_end" {
			continue
		}
		n, err := toIntPtr(canonical[key], key)
		if err != nil {
			return nil, err
		}
		if key == "line_start" {
			newStart = n
		} else {
			newEnd = n
		}
	}
	if err := validateLineRange(newStart, newEnd); err != nil {
		return nil, err
	}

	ts := time.Now().UnixMilli()
	sets = append(sets, "updated_at = ?")
	args = append(args, ts)

	switch {
	case nextStatus != nil && *nextStatus == "done" && existing.Status != "done":
		sets = append(sets, "closed_at = ?")
		args = append(args, ts)
	case nextStatus != nil && *nextStatus != "done" && existing.Status == "done":
		sets = append(sets, "closed_at = ?")
		args = append(args, nil)
	}

	args = append(args, id)
	if _, err := db.Exec(`UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update task %q: %w", id, err)
	}

	return s.GetTask(id)
}

// DeleteTask removes a task, reporting whether a row was deleted.
func (s *Store) DeleteTask(id string) (bool, error) {
	db, err := s.conn()
	if err != nil {
		return false, err
	}
	res, err := db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete task %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete task %q: %w", id, err)
	}
	return affected > 0, nil
}

// AppendNotes appends a line to a task's agent notes.
//
// The append happens in a single UPDATE statement. Reading the current notes
// first and writing them back would let two concurrent appends overwrite each
// other, so the database performs the concatenation itself.
//
// The task's updated_at is bumped, exactly as a regular update would, and
// closed_at is left untouched: appending a note never changes the status.
// Returns (nil, nil) when the task does not exist.
func (s *Store) AppendNotes(id, note string) (*Task, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	ts := time.Now().UnixMilli()
	res, err := db.Exec(`
		UPDATE tasks
		SET agent_notes = CASE
				WHEN agent_notes IS NULL OR agent_notes = '' THEN ?
				ELSE agent_notes || char(10) || ?
			END,
			updated_at = ?
		WHERE id = ?`, note, note, ts, id)
	if err != nil {
		return nil, fmt.Errorf("append notes to task %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("append notes to task %q: %w", id, err)
	}
	if affected == 0 {
		return nil, nil
	}
	return s.GetTask(id)
}

// Ping verifies that the database is open and that its schema can be read.
// The HTTP health endpoint uses it so a service that cannot serve data is
// never reported as healthy.
func (s *Store) Ping() error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	var id sql.NullString
	if err := db.QueryRow(`SELECT id FROM tasks LIMIT 1`).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read database: %w", err)
	}
	return nil
}

// ------------------------------------------------------------- utilities --

// canonicalize maps every alias key onto its column name. Unrecognised keys are
// dropped, so unknown input fields are ignored everywhere. "id" is not a
// writable field: an explicit id would let a caller overwrite an existing task.
//
// When both spellings of one column are supplied, snake_case always wins. Go
// randomises map iteration order, so a last-write-wins merge would otherwise
// make the result non-deterministic.
func canonicalize(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))

	// Two ordered passes make the winner independent of Go's randomised map
	// iteration: snake_case is applied first and therefore always beats
	// camelCase. Classification is explicit rather than relying on "_" sorting
	// before uppercase letters - it does not (0x5F > 'Z'), and ASCII ordering is
	// too incidental to hang correctness on.
	for _, snake := range []bool{true, false} {
		for key, value := range data {
			if key == "id" {
				continue
			}
			column, ok := fieldAliases[key]
			if !ok || isSnakeAlias(key) != snake {
				continue
			}
			if _, exists := out[column]; exists {
				continue // an earlier pass already won this column
			}
			out[column] = value
		}
	}
	return out
}

// isSnakeAlias reports whether an alias key uses the snake_case spelling. None
// of the camelCase aliases contain an underscore, so a single scan classifies
// them unambiguously.
func isSnakeAlias(key string) bool {
	return strings.Contains(key, "_")
}

// requireString accepts only a string, *string or json.Number for text fields.
// Composite values are rejected instead of being coerced with fmt.Sprint, which
// would silently persist values such as "map[a:1]" as a project path.
// An explicit nil is returned as ("", false) so callers can apply their own
// required-field rule.
func requireString(value any, field string) (string, bool, error) {
	switch v := value.(type) {
	case nil:
		return "", false, nil
	case string:
		return v, true, nil
	case *string:
		if v == nil {
			return "", false, nil
		}
		return *v, true, nil
	case json.Number:
		return v.String(), true, nil
	default:
		return "", false, newValidationError("Invalid %s: expected a string, got %T", field, value)
	}
}

// scalarString coerces a value for the low-risk enum fields (status, priority).
// A whole JSON number is a plausible mistake worth reporting; anything else is
// stringified rather than rejected, matching the permissive Node behaviour.
func scalarString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case *string:
		if v == nil {
			return ""
		}
		return *v
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// requiredString reads a text field from a canonicalised input map. Absent and
// explicit-null both yield "", so blank results are caught by the caller's own
// required-field check.
func requiredString(data map[string]any, field string) (string, error) {
	value, ok := data[field]
	if !ok {
		return "", nil
	}
	s, _, err := requireString(value, field)
	return s, err
}

func normalizeStatus(value any, fallback string) (string, error) {
	raw := strings.TrimSpace(scalarString(value))
	if raw == "" {
		if fallback == "" {
			return "", newValidationError("Invalid status: value is required")
		}
		return fallback, nil
	}
	lowered := strings.ToLower(raw)
	for _, s := range Statuses {
		if s == lowered {
			return lowered, nil
		}
	}
	return "", newValidationError("Invalid status %q. Expected one of: %s", value, strings.Join(Statuses, ", "))
}

func normalizePriority(value any, fallback string) (string, error) {
	raw := strings.TrimSpace(scalarString(value))
	if raw == "" {
		if fallback == "" {
			return "", newValidationError("Invalid priority: value is required")
		}
		return fallback, nil
	}
	lowered := strings.ToLower(raw)
	for _, p := range Priorities {
		if p == lowered {
			return lowered, nil
		}
	}
	return "", newValidationError("Invalid priority %q. Expected one of: %s", value, strings.Join(Priorities, ", "))
}

func validateLineRange(start, end *int64) error {
	if start != nil && end != nil && *end < *start {
		return newValidationError("Invalid line range: line_end (%d) must not be less than line_start (%d)", *end, *start)
	}
	return nil
}
