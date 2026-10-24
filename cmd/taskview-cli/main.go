package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"taskview/internal/db"
)

var version = "dev"

const (
	ExitSuccess         = 0
	ExitValidationError = 1
	ExitNotFound        = 2
	ExitInternalError   = 3
)

func pathsMatch(p1, p2 string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(p1, p2)
	}
	return p1 == p2
}

type CLIError struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
}

type HealthResponse struct {
	Success bool   `json:"success"`
	Version string `json:"version"`
}

var writableColumns = map[string]bool{
	"title":        true,
	"description":  true,
	"status":       true,
	"priority":     true,
	"file_path":    true,
	"line_start":   true,
	"line_end":     true,
	"code_snippet": true,
	"agent_notes":  true,
	"project_path": true,
	"project_name": true,
}

var readOnlyColumns = map[string]bool{
	"id":               true,
	"created_at":       true,
	"updated_at":       true,
	"closed_at":        true,
	"rejection_reason": true,
}

// jsonFieldAliases is the exact set of field names accepted by the strict
// --json contract, mapped onto the column they describe. The two accepted
// spellings of a field are the documented snake_case and camelCase form; any
// other key is an error, never a silently dropped field. Both spellings of one
// field in the same payload are refused as well.
var jsonFieldAliases = map[string]string{
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

func printHelp(w io.Writer) {
	fmt.Fprint(w, `taskview-cli - Native command-line interface for TaskView

Usage:
  taskview-cli [command] [options]

Commands:
  health                         Check database availability and connectivity
  tasks list                     List tasks for a project
  tasks get <id>                 Get task details by ID
  tasks create                   Create a new task
  tasks update <id>              Update an existing task
  tasks notes <id> [note]        Append notes to a task
  tasks reject <id> [reason]     Reject a task and roll back its status
  version                        Show CLI version
  help                           Show help information

Global Options:
  --db <path>                    Path to tasks.db (defaults to $TASKVIEW_HOME/tasks.db or ~/.taskview/tasks.db)
  --help, -h                     Show help information
  --version, -v                  Show version

Tasks List Options:
  --project-path <path>          Absolute project path (required)
  --status <status>              Filter by status (backlog|todo|in_progress|review|done)
  --priority <priority>          Filter by priority (low|medium|high|urgent)
  --limit <n>                    Maximum number of tasks to return (1-500, default 500)

Tasks Create Options:
  --project-path <path>          Absolute project path (required)
  --title <title>                Task title (required)
  --description <text>           Task description
  --status <status>              Task status (default: todo)
  --priority <priority>          Task priority (default: medium)
  --file-path <path>             Related file path
  --line-start <n>               Starting line number
  --line-end <n>                 Ending line number
  --code-snippet <text>          Code snippet
  --agent-notes <text>           Agent notes
  --project-name <name>          Project name (derived from path if omitted)
  --json <json_string>           Complete creation payload as a JSON object

Tasks Update Options:
  --id <id>                      Task ID (or specify as positional argument)
  --title <title>                Update task title
  --description <text>           Update task description
  --status <status>              Update task status
  --priority <priority>          Update task priority
  --file-path <path>             Update file path
  --line-start <n>               Update starting line number
  --line-end <n>                 Update ending line number
  --code-snippet <text>          Update code snippet
  --agent-notes <text>           Update agent notes
  --project-path <path>          Move the task to another project (must be absolute)
  --project-name <name>          Update project name
  --verify-project-path <path>   Verify the task belongs to this project before updating
  --json <json_string>           Complete update payload as a JSON object (supports null to clear fields)

Tasks Get Options:
  --id <id>                      Task ID (or specify as positional argument)
  --project-path <path>          Verify the task belongs to this project (read-only check)
  --verify-project-path <path>   Same read-only check; giving both spellings is refused

Tasks Notes Options:
  --id <id>                      Task ID (or specify as first positional argument)
  --note <text>                  Note text to append (or specify as second positional argument)
  --json <json_string>           Payload containing {"note":"..."} and optionally {"id":"..."}

Tasks Reject Options:
  --id <id>                      Task ID (or specify as first positional argument)
  --reason <text>                Rejection reason (or specify as second positional argument)
  --json <json_string>           Payload containing {"reason":"..."} and optionally {"id":"..."}

Every value may be given only once: a flag, a positional argument and the
--json payload may not carry the same meaning at the same time.

Exit Codes:
  0                              Success
  1                              Validation / parameter error
  2                              Not found
  3                              Internal / database error
`)
}

func emitJSON(w io.Writer, val any) error {
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

func emitError(w io.Writer, exitCode int, err error) int {
	var codeStr string
	switch exitCode {
	case ExitValidationError:
		codeStr = "validation_error"
	case ExitNotFound:
		codeStr = "not_found"
	case ExitInternalError:
		codeStr = "internal_error"
	default:
		codeStr = "error"
	}

	payload := CLIError{
		Success: false,
		Error:   err.Error(),
		Code:    codeStr,
	}
	data, _ := json.Marshal(payload)
	fmt.Fprintln(w, string(data))
	return exitCode
}

type parsedFlags struct {
	flags     map[string]string
	present   map[string]bool
	valueless map[string]bool
	multiples map[string]int
	args      []string
}

// booleanFlags never carry a value. They are control options: they are only
// honoured after the whole command line has been parsed and validated, so a
// value can never be swallowed by one of them and a mistyped command is never
// hidden behind a help screen.
var booleanFlags = map[string]bool{
	"help":    true,
	"version": true,
}

// flagMeanings maps a flag name to the meaning it carries. Two spellings may
// carry the same meaning (--project-path and --projectPath): giving both is a
// mistake, because the caller would not know which one wins.
var flagMeanings = map[string]string{
	"db":                  "db",
	"help":                "help",
	"version":             "version",
	"project-path":        "project-path",
	"projectPath":         "project-path",
	"status":              "status",
	"priority":            "priority",
	"limit":               "limit",
	"id":                  "id",
	"title":               "title",
	"description":         "description",
	"file-path":           "file-path",
	"line-start":          "line-start",
	"line-end":            "line-end",
	"code-snippet":        "code-snippet",
	"agent-notes":         "agent-notes",
	"project-name":        "project-name",
	"verify-project-path": "verify-project-path",
	"json":                "json",
	"note":                "note",
	"reason":              "reason",
}

// isLongFlag reports whether an argument is a long option ("--title").
func isLongFlag(arg string) bool {
	return len(arg) > 2 && strings.HasPrefix(arg, "--")
}

// isShortFlag reports whether an argument is a short option ("-h"). A single
// dash followed by a digit is a value ("-1"), not an option.
func isShortFlag(arg string) bool {
	if len(arg) != 2 || !strings.HasPrefix(arg, "-") {
		return false
	}
	c := arg[1]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// nextValue returns the value that follows a flag, if the next argument can
// be one. Another option or the "--" separator ends the flag: the value of a
// flag is never the name of another flag.
func nextValue(rawArgs []string, i int) (string, bool) {
	if i+1 >= len(rawArgs) {
		return "", false
	}
	next := rawArgs[i+1]
	if next == "--" || isLongFlag(next) || isShortFlag(next) {
		return "", false
	}
	return next, true
}

// normalizeShortFlag maps the short options onto their long names.
func normalizeShortFlag(name string) string {
	switch name {
	case "h":
		return "help"
	case "v":
		return "version"
	}
	return name
}

// parseArgs splits the command line into flags and positional arguments.
//
// The result records not only the value of a flag but also whether the flag
// was given at all and whether it was given without a value. The difference
// matters: a flag without a value is a mistake that has to be reported, and a
// flag that was given with an empty value still counts as given.
func parseArgs(rawArgs []string) (*parsedFlags, error) {
	p := &parsedFlags{
		flags:     make(map[string]string),
		present:   make(map[string]bool),
		valueless: make(map[string]bool),
		multiples: make(map[string]int),
		args:      make([]string, 0),
	}

	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		if arg == "--" {
			// Everything after the separator is data, never an option.
			p.args = append(p.args, rawArgs[i+1:]...)
			break
		}
		if isLongFlag(arg) {
			name := strings.TrimPrefix(arg, "--")
			value := ""
			hasValue := false
			if eqIdx := strings.Index(name, "="); eqIdx >= 0 {
				value = name[eqIdx+1:]
				name = name[:eqIdx]
				hasValue = true
			}
			if name == "" {
				return nil, db.NewValidationError("invalid flag %q", arg)
			}
			// A control option never carries a value. "--help=x" is a
			// mistyped command, not a help request.
			if hasValue && booleanFlags[name] {
				return nil, db.NewValidationError("flag --%s does not take a value", name)
			}
			if !hasValue && !booleanFlags[name] {
				if next, ok := nextValue(rawArgs, i); ok {
					value = next
					hasValue = true
					i++
				}
			}
			p.addFlag(name, value, hasValue)
			continue
		}
		if isShortFlag(arg) {
			name := normalizeShortFlag(strings.TrimPrefix(arg, "-"))
			value := ""
			hasValue := false
			if !booleanFlags[name] {
				if next, ok := nextValue(rawArgs, i); ok {
					value = next
					hasValue = true
					i++
				}
			}
			p.addFlag(name, value, hasValue)
			continue
		}
		p.args = append(p.args, arg)
	}

	for _, name := range sortedFlagNames(p.multiples) {
		if p.multiples[name] > 1 {
			return nil, db.NewValidationError("duplicate flag: --%s", name)
		}
	}

	return p, nil
}

// addFlag records a flag. A flag that was given without any value at all is
// remembered as valueless: that is a mistake the caller has to be told about,
// never a value that may reach the database.
func (p *parsedFlags) addFlag(name, value string, hasValue bool) {
	p.present[name] = true
	p.multiples[name]++
	if !hasValue {
		p.valueless[name] = true
		p.flags[name] = ""
		return
	}
	p.flags[name] = value
}

// sortedFlagNames returns the names of a flag map in a stable order, so that
// an error always names the same flag first.
func sortedFlagNames(counts map[string]int) []string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedPresentNames returns the names of the flags that were used, in a
// stable order.
func (p *parsedFlags) sortedPresentNames() []string {
	names := make([]string, 0, len(p.present))
	for name := range p.present {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Command contracts
// ---------------------------------------------------------------------------

// globalFlags are accepted by every command.
var globalFlags = flagSet("db", "help", "version")

// commandSpec describes what one command accepts: the flags it knows, whether
// its first positional argument is the task ID, how many positional arguments
// follow it and which flags carry a field of the task (those conflict with
// --json).
type commandSpec struct {
	key          string
	allowedFlags map[string]bool
	meanings     map[string]string
	idPositional bool
	maxExtra     int
	payloadFlags []string
}

func flagSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

var (
	specHealth = &commandSpec{key: "health", allowedFlags: flagSet()}

	specList = &commandSpec{
		key:          "list",
		allowedFlags: flagSet("project-path", "projectPath", "status", "priority", "limit"),
	}

	// Reading a task never changes it: --project-path is a check here, not a
	// move. It shares its meaning with --verify-project-path, so giving both
	// is refused instead of one of them winning silently.
	specGet = &commandSpec{
		key:          "get",
		allowedFlags: flagSet("id", "project-path", "verify-project-path"),
		meanings: map[string]string{
			"project-path":        "verify-project-path",
			"verify-project-path": "verify-project-path",
		},
		idPositional: true,
	}

	specCreate = &commandSpec{
		key:          "create",
		allowedFlags: flagSet("project-path", "projectPath", "title", "description", "status", "priority", "file-path", "line-start", "line-end", "code-snippet", "agent-notes", "project-name", "json"),
		payloadFlags: []string{"title", "description", "status", "priority", "file-path", "line-start", "line-end", "code-snippet", "agent-notes", "project-path", "projectPath", "project-name"},
	}

	// Moving a task to another project is a deliberate act: --project-path
	// writes the new project, --verify-project-path only checks the current
	// one. The two meanings are distinct, so both may be used together.
	specUpdate = &commandSpec{
		key:          "update",
		allowedFlags: flagSet("id", "title", "description", "status", "priority", "file-path", "line-start", "line-end", "code-snippet", "agent-notes", "project-path", "projectPath", "project-name", "verify-project-path", "json"),
		idPositional: true,
		payloadFlags: []string{"title", "description", "status", "priority", "file-path", "line-start", "line-end", "code-snippet", "agent-notes", "project-path", "projectPath", "project-name"},
	}

	specNotes = &commandSpec{
		key:          "notes",
		allowedFlags: flagSet("id", "note", "json"),
		idPositional: true,
		maxExtra:     1,
	}

	specReject = &commandSpec{
		key:          "reject",
		allowedFlags: flagSet("id", "reason", "json"),
		idPositional: true,
		maxExtra:     1,
	}

	specHelp    = &commandSpec{key: "help", allowedFlags: flagSet()}
	specVersion = &commandSpec{key: "version", allowedFlags: flagSet()}
)

// commandSpecs maps every accepted command word to its specification.
var commandSpecs = map[string]*commandSpec{
	"health": specHealth,
	"list":   specList,
	"ls":     specList,
	"get":    specGet,
	"create": specCreate,
	"add":    specCreate,
	"update": specUpdate,
	"edit":   specUpdate,
	"notes":  specNotes,
	"note":   specNotes,
	"reject": specReject,
}

// resolveCommand determines the command and returns its specification
// together with the positional arguments that belong to it.
func resolveCommand(p *parsedFlags) (*commandSpec, []string, error) {
	if len(p.args) == 0 {
		return nil, nil, nil
	}
	switch p.args[0] {
	case "help":
		return specHelp, p.args[1:], nil
	case "version":
		return specVersion, p.args[1:], nil
	case "tasks":
		if len(p.args) < 2 {
			// A control option still wins over the missing subcommand: it
			// was parsed, not guessed.
			if p.present["help"] {
				return specHelp, nil, nil
			}
			if p.present["version"] {
				return specVersion, nil, nil
			}
			return nil, nil, db.NewValidationError("missing tasks subcommand (list, get, create, update, notes, reject)")
		}
		spec, ok := commandSpecs[p.args[1]]
		if !ok {
			return nil, nil, db.NewValidationError("unknown tasks subcommand %q", p.args[1])
		}
		return spec, p.args[2:], nil
	}
	spec, ok := commandSpecs[p.args[0]]
	if !ok {
		return nil, nil, db.NewValidationError("unknown command %q", p.args[0])
	}
	return spec, p.args[1:], nil
}

// validateCommand refuses everything the command did not ask for: unknown
// flags, flags without a value, too many positional arguments and the same
// meaning coming from two different sources.
func validateCommand(p *parsedFlags, spec *commandSpec, args []string) error {
	for _, name := range p.sortedPresentNames() {
		if spec != nil && spec.allowedFlags[name] {
			continue
		}
		if globalFlags[name] {
			continue
		}
		if spec == nil {
			return db.NewValidationError("unknown flag --%s", name)
		}
		return db.NewValidationError("unknown flag --%s for %s", name, spec.key)
	}
	if spec == nil {
		return nil
	}

	// Two spellings of the same meaning describe the same value and must not
	// be combined: the caller would not know which one wins.
	seen := make(map[string]string)
	for _, name := range p.sortedPresentNames() {
		meaning := flagMeanings[name]
		if override, ok := spec.meanings[name]; ok {
			meaning = override
		}
		if meaning == "" {
			meaning = name
		}
		if other, ok := seen[meaning]; ok {
			return db.NewValidationError("--%s and --%s describe the same value; give it once", other, name)
		}
		seen[meaning] = name
	}

	// A flag that needs a value but did not get one is a mistake, not the
	// string "true".
	for _, name := range p.sortedPresentNames() {
		if p.valueless[name] && !booleanFlags[name] {
			return db.NewValidationError("flag --%s requires a value", name)
		}
	}

	if p.present["db"] && strings.TrimSpace(p.flags["db"]) == "" {
		return db.NewValidationError("flag --db requires a path")
	}

	// The positional arguments have fixed slots: the task ID comes first and
	// the notes and reject commands accept one more argument behind it.
	// Which of the sources fills the ID slot is decided by the command
	// itself, so that a second source for the same meaning can be refused
	// there instead of silently overwriting the first one.
	maxPositional := spec.maxExtra
	if spec.idPositional {
		maxPositional = 1 + spec.maxExtra
	}
	if len(args) > maxPositional {
		return db.NewValidationError("unexpected argument %q for %s", args[maxPositional], spec.key)
	}

	// --json carries the whole payload: a field flag next to it would be
	// silently dropped.
	if p.present["json"] {
		for _, name := range spec.payloadFlags {
			if p.present[name] {
				return db.NewValidationError("cannot combine --json with flag --%s", name)
			}
		}
	}

	return nil
}

// sourceSet counts how often the same meaning was supplied, so that a value
// that arrives from two sources is refused instead of one of them winning
// silently.
type sourceSet struct {
	sources map[string][]string
}

func newSourceSet() *sourceSet {
	return &sourceSet{sources: make(map[string][]string)}
}

func (s *sourceSet) add(meaning, source string) {
	s.sources[meaning] = append(s.sources[meaning], source)
}

// conflict reports an error when a meaning was supplied more than once.
func (s *sourceSet) conflict(meaning string) error {
	found := s.sources[meaning]
	if len(found) <= 1 {
		return nil
	}
	return db.NewValidationError("%s must be given once, but it came from %s", meaning, strings.Join(found, " and "))
}

// parseNotesPayload reads the strict --json contract of the notes command.
// Only "note" and "id" exist; anything else is a mistake that has to be
// reported instead of being dropped.
func parseNotesPayload(obj map[string]any) (note string, hasNote bool, id string, hasID bool, err error) {
	for _, key := range sortedObjectKeys(obj) {
		switch key {
		case "note", "id":
		default:
			return "", false, "", false, db.NewValidationError("Unknown field %q", key)
		}
	}
	if value, ok := obj["note"]; ok {
		text, isText := value.(string)
		if !isText {
			return "", false, "", false, db.NewValidationError("note must be a string")
		}
		note, hasNote = text, true
	} else {
		return "", false, "", false, db.NewValidationError("note is required and must be a string")
	}
	if value, ok := obj["id"]; ok {
		text, isText := value.(string)
		if !isText {
			return "", false, "", false, db.NewValidationError("id must be a string")
		}
		id, hasID = text, true
	}
	return note, hasNote, id, hasID, nil
}

// parseRejectPayload reads the strict --json contract of the reject command.
// Only "reason" and "id" exist.
func parseRejectPayload(obj map[string]any) (reason string, hasReason bool, id string, hasID bool, err error) {
	for _, key := range sortedObjectKeys(obj) {
		switch key {
		case "reason", "id":
		default:
			return "", false, "", false, db.NewValidationError("Unknown field %q", key)
		}
	}
	if value, ok := obj["reason"]; ok {
		text, isText := value.(string)
		if !isText {
			return "", false, "", false, db.NewValidationError("reason must be a string")
		}
		reason, hasReason = text, true
	} else {
		return "", false, "", false, db.NewValidationError("reason is required and must be a string")
	}
	if value, ok := obj["id"]; ok {
		text, isText := value.(string)
		if !isText {
			return "", false, "", false, db.NewValidationError("id must be a string")
		}
		id, hasID = text, true
	}
	return reason, hasReason, id, hasID, nil
}

// sortedObjectKeys returns the keys of a JSON object in a stable order, so
// that an error always names the same field first.
func sortedObjectKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func parseStrictJSON(raw string) (map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, db.NewValidationError("--json payload cannot be empty")
	}

	if err := rejectDuplicateJSONKeys(trimmed); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()

	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, db.NewValidationError("Invalid JSON: %v", err)
	}

	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, db.NewValidationError("Trailing data after JSON object")
	}

	obj, ok := val.(map[string]any)
	if !ok {
		return nil, db.NewValidationError("--json payload must be a JSON object")
	}

	return obj, nil
}

// rejectDuplicateJSONKeys reads the keys of the top-level object in raw and
// reports the first key that appears twice.
//
// encoding/json keeps the last value of a repeated key without a word. A
// strict contract cannot allow a payload to describe one field twice: the
// caller would never learn that the first value was dropped. Nested values are
// skipped, because the payload fields are scalars; a nested object is reported
// as an unknown field by the caller.
func rejectDuplicateJSONKeys(raw string) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		// A malformed payload is reported by the decode that follows.
		return nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil
	}
	seen := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil
		}
		if seen[key] {
			return db.NewValidationError("field %q is given more than once", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil
		}
	}
	return nil
}

func validateAndNormalizeJSONFields(obj map[string]any, isCreate bool) (map[string]any, error) {
	// The JSON payload accepts exactly the documented field names. Only the
	// exact snake_case and camelCase spellings exist: a misspelt key is
	// reported instead of being dropped silently, and two keys that describe
	// the same field ("file_path" and "filePath") are refused instead of one
	// of them winning arbitrarily.
	canonical := make(map[string]any, len(obj))
	origin := make(map[string]string, len(obj))
	for _, rawKey := range sortedObjectKeys(obj) {
		lower := strings.ToLower(rawKey)
		if readOnlyColumns[lower] || readOnlyColumns[rawKey] {
			return nil, db.NewValidationError("Field %q is read-only and cannot be specified", rawKey)
		}
		column, ok := jsonFieldAliases[rawKey]
		if !ok {
			return nil, db.NewValidationError("Unknown field %q", rawKey)
		}
		if previous, exists := origin[column]; exists {
			return nil, db.NewValidationError("fields %q and %q describe the same value; give it once", previous, rawKey)
		}
		origin[column] = rawKey
		canonical[column] = obj[rawKey]
	}

	out := make(map[string]any)
	for key, val := range canonical {
		if !writableColumns[key] {
			continue
		}

		if val == nil {
			if key == "title" {
				return nil, db.NewValidationError("Task title is required")
			}
			if key == "project_path" {
				return nil, db.NewValidationError("project_path is required and must be an absolute path")
			}
			out[key] = nil
			continue
		}

		switch key {
		case "title":
			s, ok := val.(string)
			if !ok {
				return nil, db.NewValidationError("title must be a string")
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, db.NewValidationError("Task title is required")
			}
			out[key] = s

		case "project_path":
			s, ok := val.(string)
			if !ok {
				return nil, db.NewValidationError("project_path must be a string")
			}
			norm, err := db.NormalizeProjectPath(s)
			if err != nil {
				return nil, err
			}
			out[key] = norm

		case "project_name", "description", "file_path", "code_snippet", "agent_notes":
			s, ok := val.(string)
			if !ok {
				return nil, db.NewValidationError("%s must be a string", key)
			}
			out[key] = s

		case "status":
			s, ok := val.(string)
			if !ok {
				return nil, db.NewValidationError("status must be a string")
			}
			s = strings.TrimSpace(s)
			matched := false
			for _, st := range db.Statuses {
				if strings.EqualFold(s, st) {
					out[key] = strings.ToLower(s)
					matched = true
					break
				}
			}
			if !matched {
				return nil, db.NewValidationError("Invalid status %q. Expected one of: %s", s, strings.Join(db.Statuses, ", "))
			}

		case "priority":
			s, ok := val.(string)
			if !ok {
				return nil, db.NewValidationError("priority must be a string")
			}
			s = strings.TrimSpace(s)
			matched := false
			for _, pr := range db.Priorities {
				if strings.EqualFold(s, pr) {
					out[key] = strings.ToLower(s)
					matched = true
					break
				}
			}
			if !matched {
				return nil, db.NewValidationError("Invalid priority %q. Expected one of: %s", s, strings.Join(db.Priorities, ", "))
			}

		case "line_start", "line_end":
			switch num := val.(type) {
			case json.Number:
				n, err := num.Int64()
				if err != nil {
					return nil, db.NewValidationError("%s must be an integer", key)
				}
				if n < 1 {
					return nil, db.NewValidationError("%s must be greater than or equal to 1", key)
				}
				out[key] = n
			case int64:
				if num < 1 {
					return nil, db.NewValidationError("%s must be greater than or equal to 1", key)
				}
				out[key] = num
			case int:
				if num < 1 {
					return nil, db.NewValidationError("%s must be greater than or equal to 1", key)
				}
				out[key] = int64(num)
			case float64:
				if num != float64(int64(num)) {
					return nil, db.NewValidationError("%s must be an integer", key)
				}
				if int64(num) < 1 {
					return nil, db.NewValidationError("%s must be greater than or equal to 1", key)
				}
				out[key] = int64(num)
			default:
				return nil, db.NewValidationError("%s must be an integer", key)
			}
		}
	}

	if isCreate {
		if _, ok := out["project_path"]; !ok {
			return nil, db.NewValidationError("project_path is required and must be an absolute path")
		}
		if _, ok := out["title"]; !ok {
			return nil, db.NewValidationError("Task title is required")
		}
		if _, ok := out["status"]; !ok {
			out["status"] = db.DefaultStatus
		}
		if _, ok := out["priority"]; !ok {
			out["priority"] = db.DefaultPriority
		}
		if _, ok := out["project_name"]; !ok {
			out["project_name"] = db.ProjectNameFromPath(out["project_path"].(string))
		}
	}

	// Validate line range if both bounds are present
	var startVal, endVal *int64
	if s, ok := out["line_start"]; ok && s != nil {
		v := s.(int64)
		startVal = &v
	}
	if e, ok := out["line_end"]; ok && e != nil {
		v := e.(int64)
		endVal = &v
	}
	if startVal != nil && endVal != nil && *endVal < *startVal {
		return nil, db.NewValidationError("Invalid line range: line_end (%d) must not be less than line_start (%d)", *endVal, *startVal)
	}

	return out, nil
}

func getDBPath(p *parsedFlags) (string, error) {
	if custom := p.flags["db"]; strings.TrimSpace(custom) != "" {
		return custom, nil
	}
	return db.DefaultDBPath()
}

func executeHealth(p *parsedFlags, stdout, stderr io.Writer) int {
	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, fmt.Errorf("database is not available: %w", err))
	}
	defer store.Close()

	if err := store.Ping(); err != nil {
		return emitError(stderr, ExitInternalError, fmt.Errorf("database is not readable: %w", err))
	}

	_ = emitJSON(stdout, HealthResponse{
		Success: true,
		Version: version,
	})
	return ExitSuccess
}

func executeTasksList(p *parsedFlags, stdout, stderr io.Writer) int {
	projectPath := p.flags["project-path"]
	if projectPath == "" {
		projectPath = p.flags["projectPath"]
	}
	if projectPath == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("project_path is required and must be an absolute path"))
	}
	normPath, err := db.NormalizeProjectPath(projectPath)
	if err != nil {
		return emitError(stderr, ExitValidationError, err)
	}

	status := p.flags["status"]
	if p.present["status"] && strings.TrimSpace(status) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("flag --status requires a value"))
	}
	if strings.Contains(status, ",") {
		return emitError(stderr, ExitValidationError, db.NewValidationError("status filter only allows a single value"))
	}
	if status != "" {
		matched := false
		for _, s := range db.Statuses {
			if strings.EqualFold(status, s) {
				status = strings.ToLower(s)
				matched = true
				break
			}
		}
		if !matched {
			return emitError(stderr, ExitValidationError, db.NewValidationError("Invalid status %q. Expected one of: %s", status, strings.Join(db.Statuses, ", ")))
		}
	}

	priority := p.flags["priority"]
	if p.present["priority"] && strings.TrimSpace(priority) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("flag --priority requires a value"))
	}
	if strings.Contains(priority, ",") {
		return emitError(stderr, ExitValidationError, db.NewValidationError("priority filter only allows a single value"))
	}
	if priority != "" {
		matched := false
		for _, pr := range db.Priorities {
			if strings.EqualFold(priority, pr) {
				priority = strings.ToLower(pr)
				matched = true
				break
			}
		}
		if !matched {
			return emitError(stderr, ExitValidationError, db.NewValidationError("Invalid priority %q. Expected one of: %s", priority, strings.Join(db.Priorities, ", ")))
		}
	}

	limit := db.DefaultLimit
	if p.present["limit"] && strings.TrimSpace(p.flags["limit"]) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("flag --limit requires a value"))
	}
	if limitStr := p.flags["limit"]; limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n < db.MinListLimit || n > db.MaxListLimit {
			return emitError(stderr, ExitValidationError, db.NewValidationError("limit must be an integer between %d and %d", db.MinListLimit, db.MaxListLimit))
		}
		limit = n
	}

	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	defer store.Close()

	opts := db.ListOptions{
		ProjectPath: normPath,
		Status:      status,
		Priority:    priority,
		Limit:       limit,
	}

	tasks, err := store.ListTasks(opts)
	if err != nil {
		if db.IsValidationError(err) {
			return emitError(stderr, ExitValidationError, err)
		}
		return emitError(stderr, ExitInternalError, err)
	}
	if tasks == nil {
		tasks = []db.Task{}
	}

	_ = emitJSON(stdout, tasks)
	return ExitSuccess
}

func executeTasksGet(p *parsedFlags, args []string, stdout, stderr io.Writer) int {
	sources := newSourceSet()
	var id string
	if len(args) > 0 {
		id = args[0]
		sources.add("id", "the positional task ID")
	}
	if p.present["id"] {
		id = p.flags["id"]
		sources.add("id", "--id")
	}
	if err := sources.conflict("id"); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}
	if strings.TrimSpace(id) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("task ID is required"))
	}

	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	defer store.Close()

	task, err := store.GetTask(id)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	if task == nil {
		return emitError(stderr, ExitNotFound, fmt.Errorf("Task not found: %s", id))
	}

	// Reading a task never moves it. The project path is only a check here.
	verifyPath := p.flags["verify-project-path"]
	if verifyPath == "" {
		verifyPath = p.flags["project-path"]
	}
	if (p.present["verify-project-path"] || p.present["project-path"]) && strings.TrimSpace(verifyPath) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("flag --verify-project-path requires a value"))
	}
	if verifyPath != "" {
		norm, err := db.NormalizeProjectPath(verifyPath)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		if !pathsMatch(task.ProjectPath, norm) {
			return emitError(stderr, ExitValidationError, fmt.Errorf("Task %s belongs to %s, not %s", id, task.ProjectPath, norm))
		}
	}

	_ = emitJSON(stdout, task)
	return ExitSuccess
}

func executeTasksCreate(p *parsedFlags, stdout, stderr io.Writer) int {
	var createData map[string]any

	if p.present["json"] {
		rawObj, err := parseStrictJSON(p.flags["json"])
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}

		validated, err := validateAndNormalizeJSONFields(rawObj, true)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		createData = validated
	} else {
		// Collect flags
		obj := make(map[string]any)
		if val, ok := p.flags["title"]; ok {
			obj["title"] = val
		}
		if val, ok := p.flags["project-path"]; ok {
			obj["project_path"] = val
		} else if val, ok := p.flags["projectPath"]; ok {
			obj["project_path"] = val
		}
		if val, ok := p.flags["project-name"]; ok {
			obj["project_name"] = val
		}
		if val, ok := p.flags["description"]; ok {
			obj["description"] = val
		}
		if val, ok := p.flags["status"]; ok {
			obj["status"] = val
		}
		if val, ok := p.flags["priority"]; ok {
			obj["priority"] = val
		}
		if val, ok := p.flags["file-path"]; ok {
			obj["file_path"] = val
		}
		if val, ok := p.flags["code-snippet"]; ok {
			obj["code_snippet"] = val
		}
		if val, ok := p.flags["agent-notes"]; ok {
			obj["agent_notes"] = val
		}
		if val, ok := p.flags["line-start"]; ok {
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return emitError(stderr, ExitValidationError, db.NewValidationError("line_start must be an integer"))
			}
			obj["line_start"] = n
		}
		if val, ok := p.flags["line-end"]; ok {
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return emitError(stderr, ExitValidationError, db.NewValidationError("line_end must be an integer"))
			}
			obj["line_end"] = n
		}

		validated, err := validateAndNormalizeJSONFields(obj, true)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		createData = validated
	}

	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	defer store.Close()

	task, err := store.CreateTask(createData)
	if err != nil {
		if db.IsValidationError(err) {
			return emitError(stderr, ExitValidationError, err)
		}
		return emitError(stderr, ExitInternalError, err)
	}

	_ = emitJSON(stdout, task)
	return ExitSuccess
}

func executeTasksUpdate(p *parsedFlags, args []string, stdout, stderr io.Writer) int {
	sources := newSourceSet()
	var id string
	if len(args) > 0 {
		id = args[0]
		sources.add("id", "the positional task ID")
	}
	if p.present["id"] {
		id = p.flags["id"]
		sources.add("id", "--id")
	}
	if err := sources.conflict("id"); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}
	if strings.TrimSpace(id) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("task ID is required"))
	}

	var updateData map[string]any

	if p.present["json"] {
		rawObj, err := parseStrictJSON(p.flags["json"])
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}

		validated, err := validateAndNormalizeJSONFields(rawObj, false)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		if len(validated) == 0 {
			return emitError(stderr, ExitValidationError, db.NewValidationError("No valid fields provided to update"))
		}
		updateData = validated
	} else {
		obj := make(map[string]any)
		if val, ok := p.flags["title"]; ok {
			obj["title"] = val
		}
		// --project-path moves the task to another project. The check of the
		// current project is --verify-project-path.
		if val, ok := p.flags["project-path"]; ok {
			obj["project_path"] = val
		} else if val, ok := p.flags["projectPath"]; ok {
			obj["project_path"] = val
		}
		if val, ok := p.flags["project-name"]; ok {
			obj["project_name"] = val
		}
		if val, ok := p.flags["description"]; ok {
			obj["description"] = val
		}
		if val, ok := p.flags["status"]; ok {
			obj["status"] = val
		}
		if val, ok := p.flags["priority"]; ok {
			obj["priority"] = val
		}
		if val, ok := p.flags["file-path"]; ok {
			obj["file_path"] = val
		}
		if val, ok := p.flags["code-snippet"]; ok {
			obj["code_snippet"] = val
		}
		if val, ok := p.flags["agent-notes"]; ok {
			obj["agent_notes"] = val
		}
		if val, ok := p.flags["line-start"]; ok {
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return emitError(stderr, ExitValidationError, db.NewValidationError("line_start must be an integer"))
			}
			obj["line_start"] = n
		}
		if val, ok := p.flags["line-end"]; ok {
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return emitError(stderr, ExitValidationError, db.NewValidationError("line_end must be an integer"))
			}
			obj["line_end"] = n
		}

		if len(obj) == 0 {
			return emitError(stderr, ExitValidationError, db.NewValidationError("No valid fields provided to update"))
		}

		validated, err := validateAndNormalizeJSONFields(obj, false)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		updateData = validated
	}

	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	defer store.Close()

	existing, err := store.GetTask(id)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	if existing == nil {
		return emitError(stderr, ExitNotFound, fmt.Errorf("Task not found: %s", id))
	}

	// The ownership check of an update is --verify-project-path. It refuses
	// to touch a task of another project before anything is written.
	if p.present["verify-project-path"] && strings.TrimSpace(p.flags["verify-project-path"]) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("flag --verify-project-path requires a value"))
	}
	if verifyPath := p.flags["verify-project-path"]; verifyPath != "" {
		norm, err := db.NormalizeProjectPath(verifyPath)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		if !pathsMatch(existing.ProjectPath, norm) {
			return emitError(stderr, ExitValidationError, fmt.Errorf("Task %s belongs to %s, not %s", id, existing.ProjectPath, norm))
		}
	}

	updated, err := store.UpdateTask(id, updateData)
	if err != nil {
		if db.IsValidationError(err) {
			return emitError(stderr, ExitValidationError, err)
		}
		return emitError(stderr, ExitInternalError, err)
	}

	_ = emitJSON(stdout, updated)
	return ExitSuccess
}

func executeTasksNotes(p *parsedFlags, args []string, stdout, stderr io.Writer) int {
	sources := newSourceSet()
	var id, note string

	// Every meaning may come from exactly one source: a positional
	// argument, a flag or the JSON payload. Two sources for one meaning are a
	// mistake and are reported instead of one of them winning silently.
	if p.present["json"] {
		rawObj, err := parseStrictJSON(p.flags["json"])
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		payloadNote, hasNote, payloadID, hasID, err := parseNotesPayload(rawObj)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		if hasNote {
			note = payloadNote
			sources.add("note", "--json")
		}
		if hasID {
			id = payloadID
			sources.add("id", "--json")
		}
	}

	if len(args) > 0 {
		id = args[0]
		sources.add("id", "the positional task ID")
	}
	if len(args) > 1 {
		note = args[1]
		sources.add("note", "the positional note")
	}
	if p.present["id"] {
		id = p.flags["id"]
		sources.add("id", "--id")
	}
	if p.present["note"] {
		note = p.flags["note"]
		sources.add("note", "--note")
	}

	if err := sources.conflict("id"); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}
	if err := sources.conflict("note"); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}

	if strings.TrimSpace(id) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("task ID is required"))
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("note must not be empty"))
	}

	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	defer store.Close()

	existing, err := store.GetTask(id)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	if existing == nil {
		return emitError(stderr, ExitNotFound, fmt.Errorf("Task not found: %s", id))
	}

	updated, err := store.AppendNotes(id, note)
	if err != nil {
		if db.IsValidationError(err) {
			return emitError(stderr, ExitValidationError, err)
		}
		return emitError(stderr, ExitInternalError, err)
	}

	_ = emitJSON(stdout, updated)
	return ExitSuccess
}

func executeTasksReject(p *parsedFlags, args []string, stdout, stderr io.Writer) int {
	sources := newSourceSet()
	var id, reason string

	if p.present["json"] {
		rawObj, err := parseStrictJSON(p.flags["json"])
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		payloadReason, hasReason, payloadID, hasID, err := parseRejectPayload(rawObj)
		if err != nil {
			return emitError(stderr, ExitValidationError, err)
		}
		if hasReason {
			reason = payloadReason
			sources.add("reason", "--json")
		}
		if hasID {
			id = payloadID
			sources.add("id", "--json")
		}
	}

	if len(args) > 0 {
		id = args[0]
		sources.add("id", "the positional task ID")
	}
	if len(args) > 1 {
		reason = args[1]
		sources.add("reason", "the positional reason")
	}
	if p.present["id"] {
		id = p.flags["id"]
		sources.add("id", "--id")
	}
	if p.present["reason"] {
		reason = p.flags["reason"]
		sources.add("reason", "--reason")
	}

	if err := sources.conflict("id"); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}
	if err := sources.conflict("reason"); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}

	if strings.TrimSpace(id) == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("task ID is required"))
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("reason must not be empty"))
	}

	dbPath, err := getDBPath(p)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	store, err := db.OpenStore(dbPath)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	defer store.Close()

	existing, err := store.GetTask(id)
	if err != nil {
		return emitError(stderr, ExitInternalError, err)
	}
	if existing == nil {
		return emitError(stderr, ExitNotFound, fmt.Errorf("Task not found: %s", id))
	}
	if existing.Status == "backlog" || existing.Status == "done" {
		return emitError(stderr, ExitValidationError, db.NewValidationError("cannot reject task %s with status %s", id, existing.Status))
	}

	updated, err := store.RejectTask(id, reason)
	if err != nil {
		if db.IsValidationError(err) {
			return emitError(stderr, ExitValidationError, err)
		}
		return emitError(stderr, ExitInternalError, err)
	}

	_ = emitJSON(stdout, updated)
	return ExitSuccess
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return ExitSuccess
	}

	p, err := parseArgs(args)
	if err != nil {
		return emitError(stderr, ExitValidationError, err)
	}

	spec, cmdArgs, err := resolveCommand(p)
	if err != nil {
		return emitError(stderr, ExitValidationError, err)
	}

	// The command line is validated first and the control options are
	// answered afterwards. A control option therefore never hides a mistake:
	// "update <id> --title --help" is an error, not a help screen.
	if err := validateCommand(p, spec, cmdArgs); err != nil {
		return emitError(stderr, ExitValidationError, err)
	}

	if p.present["version"] {
		fmt.Fprintf(stdout, "taskview-cli %s\n", version)
		return ExitSuccess
	}
	if p.present["help"] {
		printHelp(stdout)
		return ExitSuccess
	}

	if spec == nil {
		printHelp(stdout)
		return ExitSuccess
	}

	switch spec.key {
	case "help":
		printHelp(stdout)
		return ExitSuccess
	case "version":
		fmt.Fprintf(stdout, "taskview-cli %s\n", version)
		return ExitSuccess
	case "health":
		return executeHealth(p, stdout, stderr)
	case "list":
		return executeTasksList(p, stdout, stderr)
	case "get":
		return executeTasksGet(p, cmdArgs, stdout, stderr)
	case "create":
		return executeTasksCreate(p, stdout, stderr)
	case "update":
		return executeTasksUpdate(p, cmdArgs, stdout, stderr)
	case "notes":
		return executeTasksNotes(p, cmdArgs, stdout, stderr)
	case "reject":
		return executeTasksReject(p, cmdArgs, stdout, stderr)
	}

	return emitError(stderr, ExitValidationError, db.NewValidationError("unknown command %q", spec.key))
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
