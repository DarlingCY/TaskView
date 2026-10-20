package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

var (
	lifecycleEnsureBackgroundServer = EnsureBackgroundServer
	lifecycleManagedServerStatus    = ManagedServerStatus
	lifecycleStopBackgroundServer   = StopBackgroundServer
	lifecycleRunManagedServer       = RunManagedServer
	lifecycleInstall                = Install
	lifecycleUninstall              = Uninstall
	lifecycleOpenBrowser            = OpenBrowser
)

// ColorConfig provides ANSI styling or no-ops depending on TTY / environment.
type ColorConfig struct {
	Reset   string
	Dim     string
	Bold    string
	Red     string
	Green   string
	Yellow  string
	Blue    string
	Magenta string
	Cyan    string
	Gray    string
}

func initColors(w io.Writer) ColorConfig {
	useColor := true
	if _, has := os.LookupEnv("NO_COLOR"); has {
		useColor = false
	} else if _, has := os.LookupEnv("TASKVIEW_NO_COLOR"); has {
		useColor = false
	} else if f, ok := w.(*os.File); ok {
		if !isatty.IsTerminal(f.Fd()) && !isatty.IsCygwinTerminal(f.Fd()) {
			useColor = false
		}
	} else {
		useColor = false
	}

	if !useColor {
		return ColorConfig{}
	}
	return ColorConfig{
		Reset:   "\x1b[0m",
		Dim:     "\x1b[2m",
		Bold:    "\x1b[1m",
		Red:     "\x1b[31m",
		Green:   "\x1b[32m",
		Yellow:  "\x1b[33m",
		Blue:    "\x1b[34m",
		Magenta: "\x1b[35m",
		Cyan:    "\x1b[36m",
		Gray:    "\x1b[90m",
	}
}

func statusIcon(status string) string {
	switch status {
	case "backlog":
		return "[ ]"
	case "todo":
		return "[>]"
	case "in_progress":
		return "[~]"
	case "done":
		return "[x]"
	default:
		return "[ ]"
	}
}

func colorStatus(status string, c ColorConfig) string {
	switch status {
	case "done":
		return c.Green
	case "in_progress":
		return c.Cyan
	case "todo":
		return c.Yellow
	default:
		return c.Gray
	}
}

func colorPriority(priority string, c ColorConfig) string {
	switch priority {
	case "urgent":
		return c.Red + c.Bold
	case "high":
		return c.Red
	case "medium":
		return c.Yellow
	default:
		return c.Gray
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func displayWidth(text string) int {
	plain := ansiRe.ReplaceAllString(text, "")
	width := 0
	for _, ch := range plain {
		if (ch >= 0x1100 && ch <= 0x115f) || ch > 0x2e80 {
			width += 2
		} else {
			width += 1
		}
	}
	return width
}

func padEnd(text string, width int) string {
	diff := width - displayWidth(text)
	if diff > 0 {
		return text + strings.Repeat(" ", diff)
	}
	return text
}

func shortID(id string) string {
	parts := strings.Split(id, "_")
	if len(parts) > 2 {
		return fmt.Sprintf("%s_%s_%s", parts[0], parts[1], parts[2])
	}
	return id
}

func locationLabel(task *Task) string {
	if task.FilePath == nil || *task.FilePath == "" {
		return ""
	}
	fp := *task.FilePath
	if task.LineStart != nil && task.LineEnd != nil {
		if *task.LineStart == *task.LineEnd {
			return fmt.Sprintf("%s:%d", fp, *task.LineStart)
		}
		return fmt.Sprintf("%s:%d-%d", fp, *task.LineStart, *task.LineEnd)
	}
	return fp
}

func relativeTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	diff := time.Now().UnixMilli() - ts
	min := diff / 60000
	if min < 1 {
		return "just now"
	}
	if min < 60 {
		return fmt.Sprintf("%dm ago", min)
	}
	hrs := min / 60
	if hrs < 24 {
		return fmt.Sprintf("%dh ago", hrs)
	}
	days := hrs / 24
	if days < 30 {
		return fmt.Sprintf("%dd ago", days)
	}
	return time.UnixMilli(ts).UTC().Format("2006-01-02")
}

func indent(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func renderList(w io.Writer, tasks []Task, showProject bool, c ColorConfig) {
	if len(tasks) == 0 {
		fmt.Fprintf(w, "%sNo tasks found.%s\n", c.Dim, c.Reset)
		return
	}

	idW := 2
	statusW := 6
	prioW := 8
	projW := 4
	titleW := 5
	maxLocW := 4

	for _, t := range tasks {
		if d := displayWidth(shortID(t.ID)); d > idW {
			idW = d
		}
		if d := displayWidth(t.Status); d > statusW {
			statusW = d
		}
		if d := displayWidth(t.Priority); d > prioW {
			prioW = d
		}
		if d := displayWidth(t.ProjectName); d > projW {
			projW = d
		}
		if d := displayWidth(t.Title); d > titleW {
			titleW = d
		}
		if d := displayWidth(locationLabel(&t)); d > maxLocW {
			maxLocW = d
		}
	}

	if titleW > 48 {
		titleW = 48
	}
	fileW := maxLocW
	if fileW < 4 {
		fileW = 4
	}
	if fileW > 28 {
		fileW = 28
	}

	var headerParts []string
	headerParts = append(headerParts, padEnd("ID", idW))
	headerParts = append(headerParts, padEnd("STATUS", statusW))
	headerParts = append(headerParts, padEnd("PRIORITY", prioW))
	if showProject {
		headerParts = append(headerParts, padEnd("PROJECT", projW))
	}
	headerParts = append(headerParts, padEnd("TITLE", titleW))
	headerParts = append(headerParts, padEnd("FILE", fileW))
	header := strings.Join(headerParts, "  ")
	fmt.Fprintf(w, "%s%s%s\n", c.Dim, header, c.Reset)

	for _, task := range tasks {
		sc := colorStatus(task.Status, c)
		pc := colorPriority(task.Priority, c)
		title := task.Title
		if displayWidth(title) > titleW {
			runes := []rune(title)
			if len(runes) > titleW-1 && titleW > 1 {
				title = string(runes[:titleW-1]) + "…"
			} else {
				title = string(runes) + "…"
			}
		}

		loc := locationLabel(&task)
		locText := loc
		locRunes := []rune(loc)
		if len(locRunes) > 28 {
			locText = "…" + string(locRunes[len(locRunes)-27:])
		}

		var rowParts []string
		rowParts = append(rowParts, padEnd(shortID(task.ID), idW))
		statusCol := fmt.Sprintf("%s%s %s%s", sc, statusIcon(task.Status), task.Status, c.Reset)
		rowParts = append(rowParts, padEnd(statusCol, statusW))
		prioCol := fmt.Sprintf("%s%s%s", pc, task.Priority, c.Reset)
		rowParts = append(rowParts, padEnd(prioCol, prioW))
		if showProject {
			rowParts = append(rowParts, padEnd(task.ProjectName, projW))
		}
		titleCol := title
		if task.Status == "done" {
			titleCol = fmt.Sprintf("%s%s%s", c.Dim, title, c.Reset)
		}
		rowParts = append(rowParts, padEnd(titleCol, titleW))
		locCol := fmt.Sprintf("%s%s%s", c.Dim, padEnd(locText, fileW), c.Reset)
		rowParts = append(rowParts, locCol)

		fmt.Fprintln(w, strings.Join(rowParts, "  "))
	}

	counts := make(map[string]int)
	for _, t := range tasks {
		counts[t.Status]++
	}
	var summaries []string
	for _, s := range Statuses {
		if cnt := counts[s]; cnt > 0 {
			summaries = append(summaries, fmt.Sprintf("%d %s", cnt, s))
		}
	}
	summary := strings.Join(summaries, ", ")

	sepLen := len(header)
	if sepLen > 100 {
		sepLen = 100
	}
	fmt.Fprintf(w, "%s%s%s\n", c.Dim, strings.Repeat("-", sepLen), c.Reset)
	plural := "s"
	if len(tasks) == 1 {
		plural = ""
	}
	if summary != "" {
		fmt.Fprintf(w, "%s%d task%s - %s%s\n", c.Dim, len(tasks), plural, summary, c.Reset)
	} else {
		fmt.Fprintf(w, "%s%d task%s%s\n", c.Dim, len(tasks), plural, c.Reset)
	}
}

func renderTask(w io.Writer, task *Task, c ColorConfig) {
	sc := colorStatus(task.Status, c)
	pc := colorPriority(task.Priority, c)
	tagList := TagsToList(task.Tags)

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s%s%s\n", c.Bold, task.Title, c.Reset)
	barLen := displayWidth(task.Title)
	if barLen < 10 {
		barLen = 10
	} else if barLen > 72 {
		barLen = 72
	}
	fmt.Fprintf(w, "%s%s%s\n", c.Dim, strings.Repeat("=", barLen), c.Reset)
	fmt.Fprintf(w, "ID        %s%s%s\n", c.Cyan, task.ID, c.Reset)
	fmt.Fprintf(w, "Status    %s%s %s%s\n", sc, statusIcon(task.Status), task.Status, c.Reset)
	fmt.Fprintf(w, "Priority  %s%s%s\n", pc, task.Priority, c.Reset)
	fmt.Fprintf(w, "Project   %s %s%s%s\n", task.ProjectName, c.Dim, task.ProjectPath, c.Reset)
	if loc := locationLabel(task); loc != "" {
		fmt.Fprintf(w, "File      %s\n", loc)
	}
	if len(tagList) > 0 {
		var tagsFormatted []string
		for _, t := range tagList {
			tagsFormatted = append(tagsFormatted, fmt.Sprintf("%s#%s%s", c.Magenta, t, c.Reset))
		}
		fmt.Fprintf(w, "Tags      %s\n", strings.Join(tagsFormatted, " "))
	}
	fmt.Fprintf(w, "Created   %s%s (%s)%s\n", c.Dim, time.UnixMilli(task.CreatedAt).Local().Format("1/2/2006, 3:04:05 PM"), relativeTime(task.CreatedAt), c.Reset)
	fmt.Fprintf(w, "Updated   %s%s (%s)%s\n", c.Dim, time.UnixMilli(task.UpdatedAt).Local().Format("1/2/2006, 3:04:05 PM"), relativeTime(task.UpdatedAt), c.Reset)
	if task.ClosedAt != nil {
		fmt.Fprintf(w, "Closed    %s%s%s\n", c.Dim, time.UnixMilli(*task.ClosedAt).Local().Format("1/2/2006, 3:04:05 PM"), c.Reset)
	}

	if task.Description != nil && *task.Description != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%sDescription%s\n", c.Bold, c.Reset)
		fmt.Fprintln(w, indent(*task.Description, "  "))
	}
	if task.CodeSnippet != nil && *task.CodeSnippet != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%sCode snippet%s\n", c.Bold, c.Reset)
		fmt.Fprintf(w, "%s%s%s\n", c.Dim, indent(*task.CodeSnippet, "  | "), c.Reset)
	}
	if task.AgentNotes != nil && *task.AgentNotes != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%sAgent notes%s\n", c.Bold, c.Reset)
		fmt.Fprintln(w, indent(*task.AgentNotes, "  "))
	}
	fmt.Fprintln(w)
}

func getHelp(c ColorConfig) string {
	return fmt.Sprintf(`
%sTaskView%s %sv1.0.0 - task tracking for AI coding agents%s

%sUSAGE%s
  taskview <command> [options]

%sCOMMANDS%s
  %sadd%s <title>            Create a task
  %slist%s                  List tasks for the current project
  %sget%s <id>              Show full task details
  %supdate%s <id>            Update a task
  %sdelete%s <id>            Delete a task
  %sstats%s                 Show task counts
  %sui%s                     Start or open the local web UI (background server)
  %sserver%s <command>       Manage background server (start, stop, status)
  %sinstall%s               Install TaskView executable to managed directory
  %suninstall%s             Uninstall TaskView executable and remove autostart
  %shelp%s                   Show this help

%sSERVER COMMANDS%s
  %sstart%s [--port N]       Start background server (default 3333)
  %sstop%s                   Stop running background server
  %sstatus%s                 Check background server status

%sINSTALL OPTIONS%s
  --autostart               Enable autostart on user login (Windows only)
                            (once enabled, retained across reinstalls; uninstall to remove)

%sADD OPTIONS%s
  --desc <text>              Task description
  --file <path>              Related file path
  --lines <range>            Line range, e.g. "12-30" or "42"
  --priority <level>         low | medium | high | urgent
  --status <status>          backlog | todo | in_progress | done
  --notes <text>             Agent notes
  --tags <a,b,c>             Comma separated tags
  --code <text>              Code snippet to attach
  --project <path>           Project directory (defaults to cwd)

%sLIST OPTIONS%s
  --all                      Include tasks from every project
  --status <status>          Filter by status
  --priority <level>         Filter by priority
  --search <text>            Search title/description/notes/tags
  --sort <key>               updated | created | priority | title

%sUPDATE OPTIONS%s
  --title <text>             Change the title
  --status <status>          backlog | todo | in_progress | done
  --priority <level>         low | medium | high | urgent
  --notes <text>             Replace agent notes
  --append-notes <text>      Append to agent notes
  --tags <a,b,c>             Replace tags
  --desc <text>              Replace description
  --file <path>              Replace file path
  --lines <range>            Replace line range

%sUI OPTIONS%s
  --port <number>            Port to bind (default 3333)
  --no-open                  Do not open a browser

%sEXAMPLES%s
  %staskview add "Refactor auth middleware" --priority high --tags backend,security%s
  %staskview list --status in_progress --all%s
  %staskview ui%s
  %staskview server status%s
  %staskview install --autostart%s
`,
		c.Bold, c.Reset, c.Dim, c.Reset,
		c.Bold, c.Reset,
		c.Bold, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Bold, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Cyan, c.Reset,
		c.Bold, c.Reset,
		c.Bold, c.Reset,
		c.Bold, c.Reset,
		c.Bold, c.Reset,
		c.Bold, c.Reset,
		c.Bold, c.Reset,
		c.Dim, c.Reset,
		c.Dim, c.Reset,
		c.Dim, c.Reset,
		c.Dim, c.Reset,
		c.Dim, c.Reset,
	)
}

func camelizeKey(key string) string {
	parts := strings.Split(key, "-")
	if len(parts) <= 1 {
		return key
	}
	res := parts[0]
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		res += strings.ToUpper(p[:1]) + p[1:]
	}
	return res
}

// RawParsedArgs holds options and positionals before command-specific validation.
type RawParsedArgs struct {
	Positional []string
	Flags      map[string]string
	BoolFlags  map[string]bool
}

var commandKnownFlags = map[string]map[string]bool{
	"add": {
		"desc": true, "description": true,
		"file":  true,
		"lines": true, "line": true,
		"priority": true,
		"status":   true,
		"notes":    true, "agentNotes": true, "agent_notes": true,
		"tags": true, "tag": true,
		"code": true, "snippet": true, "codeSnippet": true, "code_snippet": true,
		"project": true, "projectPath": true, "project_path": true, "C": true,
		"help": true, "h": true,
	},
	"list": {
		"all": true, "a": true,
		"status":   true,
		"priority": true,
		"search":   true, "q": true,
		"sort":    true,
		"project": true, "projectPath": true, "project_path": true, "C": true,
		"help": true, "h": true,
	},
	"get": {
		"help": true, "h": true,
	},
	"update": {
		"title":    true,
		"status":   true,
		"priority": true,
		"notes":    true, "agentNotes": true, "agent_notes": true,
		"append-notes": true, "appendNotes": true,
		"tags": true, "tag": true,
		"desc": true, "description": true,
		"file":  true,
		"lines": true, "line": true,
		"help": true, "h": true,
	},
	"delete": {
		"help": true, "h": true,
	},
	"stats": {
		"all": true, "a": true,
		"project": true, "projectPath": true, "project_path": true, "C": true,
		"help": true, "h": true,
	},
	"ui": {
		"port": true, "p": true,
		"no-open": true, "noOpen": true, "open": true,
		"help": true, "h": true,
	},
	"server": {
		"port": true, "p": true,
		"data-dir": true, "dataDir": true,
		"help": true, "h": true,
	},
	"install": {
		"autostart": true,
		"help":      true, "h": true,
	},
	"uninstall": {
		"help": true, "h": true,
	},
	"help": {
		"help": true, "h": true,
	},
	"version": {
		"version": true, "v": true,
	},
}

var serverSubcommandKnownFlags = map[string]map[string]bool{
	"start": {
		"port": true, "p": true,
		"data-dir": true, "dataDir": true,
		"help": true, "h": true,
	},
	"stop": {
		"help": true, "h": true,
	},
	"status": {
		"help": true, "h": true,
	},
	"run": {
		"port": true, "p": true,
		"data-dir": true, "dataDir": true,
		"help": true, "h": true,
	},
}

var boolOptionNames = map[string]bool{
	"all":       true,
	"a":         true,
	"no-open":   true,
	"noOpen":    true,
	"open":      true,
	"autostart": true,
	"help":      true,
	"h":         true,
	"version":   true,
	"v":         true,
}

func isNegativeNumber(s string) bool {
	return len(s) > 1 && s[0] == '-' && s[1] >= '0' && s[1] <= '9'
}

func parseCLIArgs(argv []string) (*RawParsedArgs, error) {
	flags := make(map[string]string)
	boolFlags := make(map[string]bool)
	positional := []string{}

	for i := 0; i < len(argv); i++ {
		arg := argv[i]

		if arg == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}

		if strings.HasPrefix(arg, "--") {
			key := arg[2:]
			var val *string
			if eq := strings.Index(key, "="); eq != -1 {
				v := key[eq+1:]
				key = key[:eq]
				val = &v
			}
			key = strings.TrimSpace(key)
			cKey := camelizeKey(key)

			if boolOptionNames[key] || boolOptionNames[cKey] {
				if val != nil {
					b, ok := parseBoolParam(*val)
					if !ok {
						return nil, fmt.Errorf("invalid boolean value for option \"--%s\": %q", key, *val)
					}
					boolFlags[key] = b
					boolFlags[cKey] = b
					flags[key] = *val
					flags[cKey] = *val
				} else {
					boolFlags[key] = true
					boolFlags[cKey] = true
					flags[key] = "true"
					flags[cKey] = "true"
				}
				continue
			}

			// Value option
			if val != nil {
				flags[key] = *val
				flags[cKey] = *val
			} else {
				if i+1 < len(argv) && (!strings.HasPrefix(argv[i+1], "-") || isNegativeNumber(argv[i+1])) {
					flags[key] = argv[i+1]
					flags[cKey] = argv[i+1]
					i++
				} else {
					return nil, fmt.Errorf("option \"--%s\" requires an argument", key)
				}
			}
			continue
		}

		if strings.HasPrefix(arg, "-") && len(arg) > 1 && !isNegativeNumber(arg) {
			key := arg[1:]
			var val *string
			if eq := strings.Index(key, "="); eq != -1 {
				v := key[eq+1:]
				key = key[:eq]
				val = &v
			}

			if boolOptionNames[key] {
				if val != nil {
					b, ok := parseBoolParam(*val)
					if !ok {
						return nil, fmt.Errorf("invalid boolean value for option \"-%s\": %q", key, *val)
					}
					boolFlags[key] = b
					flags[key] = *val
				} else {
					boolFlags[key] = true
					flags[key] = "true"
				}
				continue
			}

			if val != nil {
				flags[key] = *val
			} else {
				if i+1 < len(argv) && (!strings.HasPrefix(argv[i+1], "-") || isNegativeNumber(argv[i+1])) {
					flags[key] = argv[i+1]
					i++
				} else {
					return nil, fmt.Errorf("option \"-%s\" requires an argument", key)
				}
			}
			continue
		}

		positional = append(positional, arg)
	}

	return &RawParsedArgs{
		Positional: positional,
		Flags:      flags,
		BoolFlags:  boolFlags,
	}, nil
}

func hasFlag(flags map[string]string, names ...string) bool {
	for _, n := range names {
		if _, ok := flags[n]; ok {
			return true
		}
	}
	return false
}

func getFlag(flags map[string]string, names ...string) string {
	for _, n := range names {
		if v, ok := flags[n]; ok {
			return v
		}
	}
	return ""
}

func getBoolFlag(boolFlags map[string]bool, names ...string) bool {
	for _, n := range names {
		if b, ok := boolFlags[n]; ok && b {
			return true
		}
	}
	return false
}

func parsePort(portStr string) (int, error) {
	if strings.TrimSpace(portStr) == "" {
		return 0, fmt.Errorf("invalid port: %s", portStr)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 0 || p > 65535 {
		return 0, fmt.Errorf("invalid port: %s", portStr)
	}
	return p, nil
}

// Canonical command names
func canonicalCommand(cmd string) string {
	switch cmd {
	case "add", "a", "new":
		return "add"
	case "list", "ls", "l":
		return "list"
	case "get", "show", "g":
		return "get"
	case "update", "set", "u":
		return "update"
	case "delete", "rm", "remove", "d":
		return "delete"
	case "stats", "stat":
		return "stats"
	case "ui", "serve", "web":
		return "ui"
	case "server":
		return "server"
	case "install":
		return "install"
	case "uninstall":
		return "uninstall"
	case "help":
		return "help"
	case "version":
		return "version"
	default:
		return cmd
	}
}

func validateFlagsForCommand(canonicalCmd string, rawFlags map[string]string) error {
	allowed, ok := commandKnownFlags[canonicalCmd]
	if !ok {
		return nil
	}
	for k := range rawFlags {
		if !allowed[k] && !allowed[camelizeKey(k)] {
			if len(k) == 1 {
				return fmt.Errorf("unknown option \"-%s\"", k)
			}
			return fmt.Errorf("unknown option \"--%s\"", k)
		}
	}
	return nil
}

func cmdAdd(stdout, stderr io.Writer, store *Store, flags map[string]string, positional []string, c ColorConfig) int {
	title := strings.TrimSpace(strings.Join(positional, " "))
	if title == "" {
		fmt.Fprintf(stderr, "%sError:%s task title is required.\n", c.Red, c.Reset)
		fmt.Fprintf(stderr, "%sUsage: taskview add <title> [--desc \"...\"] [--priority high]%s\n", c.Dim, c.Reset)
		return 1
	}

	lineRange := getFlag(flags, "lines", "line")
	start, end, err := ParseLineRange(lineRange)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	data := map[string]any{
		"title": title,
	}
	if v := getFlag(flags, "desc", "description"); v != "" {
		data["description"] = v
	}
	if v := getFlag(flags, "status"); v != "" {
		data["status"] = v
	}
	if v := getFlag(flags, "priority"); v != "" {
		data["priority"] = v
	}
	if v := getFlag(flags, "file"); v != "" {
		data["file_path"] = v
	}
	if start != nil {
		data["line_start"] = *start
	}
	if end != nil {
		data["line_end"] = *end
	}
	if v := getFlag(flags, "code", "snippet", "codeSnippet", "code_snippet"); v != "" {
		data["code_snippet"] = v
	}
	if v := getFlag(flags, "notes", "agentNotes", "agent_notes"); v != "" {
		data["agent_notes"] = v
	}
	if v := getFlag(flags, "tags", "tag"); v != "" {
		data["tags"] = v
	}
	if proj := getFlag(flags, "project", "projectPath", "project_path", "C"); proj != "" && proj != "true" {
		data["project_path"] = proj
	}

	task, err := store.CreateTask(data)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s+%s Task created  %s%s%s\n", c.Green, c.Reset, c.Cyan, task.ID, c.Reset)
	fmt.Fprintf(stdout, "  %s%s%s\n", c.Bold, task.Title, c.Reset)
	fmt.Fprintf(stdout, "  %sstatus=%s  priority=%s  project=%s%s\n", c.Dim, task.Status, task.Priority, task.ProjectName, c.Reset)
	if task.FilePath != nil && *task.FilePath != "" {
		fmt.Fprintf(stdout, "  %sat %s%s\n", c.Dim, locationLabel(task), c.Reset)
	}
	fmt.Fprintf(stdout, "  %sview: taskview get %s%s\n", c.Dim, task.ID, c.Reset)
	fmt.Fprintln(stdout)
	return 0
}

func cmdList(stdout, stderr io.Writer, store *Store, flags map[string]string, boolFlags map[string]bool, c ColorConfig) int {
	allProjects := getBoolFlag(boolFlags, "all", "a") || getFlag(flags, "all", "a") == "true"
	opts := ListOptions{
		ProjectPath: getFlag(flags, "project", "projectPath", "project_path", "C"),
		Status:      getFlag(flags, "status"),
		Priority:    getFlag(flags, "priority"),
		Search:      getFlag(flags, "search", "q"),
		Sort:        getFlag(flags, "sort"),
		All:         allProjects,
	}

	tasks, err := store.ListTasks(opts)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	renderList(stdout, tasks, allProjects, c)
	return 0
}

func cmdGet(stdout, stderr io.Writer, store *Store, positional []string, c ColorConfig) int {
	if len(positional) == 0 || positional[0] == "" {
		fmt.Fprintf(stderr, "%sError:%s task id is required.\n", c.Red, c.Reset)
		fmt.Fprintf(stderr, "%sUsage: taskview get <id>%s\n", c.Dim, c.Reset)
		return 1
	}

	id := positional[0]
	task, err := store.GetTask(id)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}
	if task == nil {
		fmt.Fprintf(stderr, "%sError:%s task not found: %s\n", c.Red, c.Reset, id)
		return 1
	}

	renderTask(stdout, task, c)
	return 0
}

func cmdUpdate(stdout, stderr io.Writer, store *Store, flags map[string]string, positional []string, c ColorConfig) int {
	if len(positional) == 0 || positional[0] == "" {
		fmt.Fprintf(stderr, "%sError:%s task id is required.\n", c.Red, c.Reset)
		fmt.Fprintf(stderr, "%sUsage: taskview update <id> [--status done] [--priority high] [--notes \"...\"]%s\n", c.Dim, c.Reset)
		return 1
	}

	id := positional[0]
	existing, err := store.GetTask(id)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}
	if existing == nil {
		fmt.Fprintf(stderr, "%sError:%s task not found: %s\n", c.Red, c.Reset, id)
		return 1
	}

	fields := make(map[string]any)
	if v, ok := flags["title"]; ok {
		fields["title"] = v
	}
	if v, ok := flags["status"]; ok {
		fields["status"] = v
	}
	if v, ok := flags["priority"]; ok {
		fields["priority"] = v
	}
	if hasFlag(flags, "notes", "agentNotes", "agent_notes") {
		fields["agent_notes"] = getFlag(flags, "notes", "agentNotes", "agent_notes")
	}
	if appendNotes := getFlag(flags, "append-notes", "appendNotes"); appendNotes != "" {
		if existing.AgentNotes != nil && *existing.AgentNotes != "" {
			fields["agent_notes"] = *existing.AgentNotes + "\n" + appendNotes
		} else {
			fields["agent_notes"] = appendNotes
		}
	}
	if hasFlag(flags, "tags", "tag") {
		fields["tags"] = getFlag(flags, "tags", "tag")
	}
	if hasFlag(flags, "desc", "description") {
		fields["description"] = getFlag(flags, "desc", "description")
	}
	if hasFlag(flags, "file") {
		fields["file_path"] = getFlag(flags, "file")
	}
	if hasFlag(flags, "lines", "line") {
		lr := getFlag(flags, "lines", "line")
		if strings.TrimSpace(lr) == "" {
			fields["line_start"] = nil
			fields["line_end"] = nil
		} else {
			start, end, err := ParseLineRange(lr)
			if err != nil {
				fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
				return 1
			}
			if start != nil {
				fields["line_start"] = *start
			} else {
				fields["line_start"] = nil
			}
			if end != nil {
				fields["line_end"] = *end
			} else {
				fields["line_end"] = nil
			}
		}
	}

	if len(fields) == 0 {
		fmt.Fprintf(stderr, "%sError:%s nothing to update.\n", c.Red, c.Reset)
		fmt.Fprintf(stderr, "%sProvide at least one of --status, --priority, --title, --notes, --tags, --desc, --file, --lines%s\n", c.Dim, c.Reset)
		return 1
	}

	task, err := store.UpdateTask(id, fields)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s✓%s Task updated  %s%s%s\n", c.Green, c.Reset, c.Cyan, task.ID, c.Reset)
	fmt.Fprintf(stdout, "  %s%s%s\n", c.Bold, task.Title, c.Reset)
	fmt.Fprintf(stdout, "  %sstatus=%s  priority=%s  updated=%s%s\n", c.Dim, task.Status, task.Priority, relativeTime(task.UpdatedAt), c.Reset)
	fmt.Fprintln(stdout)
	return 0
}

func cmdDelete(stdout, stderr io.Writer, store *Store, positional []string, c ColorConfig) int {
	if len(positional) == 0 || positional[0] == "" {
		fmt.Fprintf(stderr, "%sError:%s task id is required.\n", c.Red, c.Reset)
		fmt.Fprintf(stderr, "%sUsage: taskview delete <id>%s\n", c.Dim, c.Reset)
		return 1
	}

	id := positional[0]
	task, err := store.GetTask(id)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}
	if task == nil {
		fmt.Fprintf(stderr, "%sError:%s task not found: %s\n", c.Red, c.Reset, id)
		return 1
	}

	deleted, err := store.DeleteTask(id)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}
	if !deleted {
		fmt.Fprintf(stderr, "%sError:%s task not found: %s\n", c.Red, c.Reset, id)
		return 1
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s-%s Deleted %s%s%s %s%s%s\n", c.Red, c.Reset, c.Cyan, id, c.Reset, c.Dim, task.Title, c.Reset)
	fmt.Fprintln(stdout)
	return 0
}

func cmdStats(stdout, stderr io.Writer, store *Store, flags map[string]string, boolFlags map[string]bool, c ColorConfig) int {
	allProjects := getBoolFlag(boolFlags, "all", "a") || getFlag(flags, "all", "a") == "true"
	project := getFlag(flags, "project", "projectPath", "project_path", "C")
	scope := ""
	if !allProjects {
		if project != "" && project != "true" {
			scope = project
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				fmt.Fprintf(stderr, "%sError:%s failed to get working directory: %s\n", c.Red, c.Reset, err.Error())
				return 1
			}
			scope = cwd
		}
	}

	stats, err := store.GetStats(scope)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	projLabel := " - all projects"
	if stats.ProjectPath != nil && *stats.ProjectPath != "" {
		projLabel = fmt.Sprintf(" - %s", *stats.ProjectPath)
	}

	pct := int64(0)
	if stats.Total > 0 {
		pct = int64(math.Round(float64(stats.Total-stats.Open) / float64(stats.Total) * 100))
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%sTask statistics%s%s%s%s\n", c.Bold, c.Reset, c.Dim, projLabel, c.Reset)
	fmt.Fprintf(stdout, "  Total       %s%d%s\n", c.Bold, stats.Total, c.Reset)
	fmt.Fprintf(stdout, "  Backlog     %d\n", stats.Backlog)
	fmt.Fprintf(stdout, "  Todo        %s%d%s\n", c.Yellow, stats.Todo, c.Reset)
	fmt.Fprintf(stdout, "  In progress %s%d%s\n", c.Cyan, stats.InProgress, c.Reset)
	fmt.Fprintf(stdout, "  Done        %s%d%s\n", c.Green, stats.Done, c.Reset)
	fmt.Fprintf(stdout, "  Open        %d %s(%d%% complete)%s\n", stats.Open, c.Dim, pct, c.Reset)
	fmt.Fprintln(stdout)
	return 0
}

func cmdInstall(stdout, stderr io.Writer, flags map[string]string, boolFlags map[string]bool, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	autostart := getBoolFlag(boolFlags, "autostart")

	result, err := lifecycleInstall(InstallOptions{Autostart: autostart})
	if err != nil {
		if result != nil && result.Path != "" {
			fmt.Fprintf(stdout, "Installed TaskView to %s\n", result.Path)
		}
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	fmt.Fprintf(stdout, "Installed TaskView to %s\n", result.Path)
	if result.Autostart {
		fmt.Fprintln(stdout, "Autostart on login enabled.")
	} else {
		fmt.Fprintln(stdout, "Autostart not enabled.")
	}
	return 0
}

func cmdUninstall(stdout, stderr io.Writer, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	dataDir, _ := DataDir()

	if err := lifecycleUninstall(); err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	scheduled := SelfDeleteScheduled()
	if scheduled != "" {
		fmt.Fprintln(stdout, "TaskView registration removed; binary removal scheduled on process exit.")
	} else {
		fmt.Fprintln(stdout, "TaskView uninstalled successfully.")
	}
	if dataDir != "" {
		fmt.Fprintf(stdout, "Data retained at %s (tasks database preserved).\n", dataDir)
	}
	return 0
}

func cmdServer(stdout, stderr io.Writer, flags map[string]string, boolFlags map[string]bool, positional []string, c ColorConfig) int {
	if len(positional) == 0 || getBoolFlag(boolFlags, "help", "h") || positional[0] == "help" {
		for k := range flags {
			if k != "help" && k != "h" {
				if len(k) == 1 {
					fmt.Fprintf(stderr, "%sError:%s unknown option \"-%s\"\n", c.Red, c.Reset, k)
				} else {
					fmt.Fprintf(stderr, "%sError:%s unknown option \"--%s\"\n", c.Red, c.Reset, k)
				}
				return 1
			}
		}

		fmt.Fprintf(stdout, `
%sUsage:%s taskview server <command> [options]

%sCommands:%s
  %sstart%s [--port N]       Start background server (default port 3333)
  %sstop%s                   Stop running background server
  %sstatus%s                 Check background server status
  %srun%s [--port N]         Run server in foreground (advanced)

%sOptions:%s
  --port <number>            Port to bind (default 3333)
  --data-dir <path>          Path to task directory (start and run commands)
`,
			c.Bold, c.Reset,
			c.Bold, c.Reset,
			c.Cyan, c.Reset,
			c.Cyan, c.Reset,
			c.Cyan, c.Reset,
			c.Cyan, c.Reset,
			c.Bold, c.Reset,
		)
		return 0
	}

	subCmd := positional[0]
	args := positional[1:]

	allowed, ok := serverSubcommandKnownFlags[subCmd]
	if !ok {
		fmt.Fprintf(stderr, "%sError:%s unknown server command \"%s\"\n", c.Red, c.Reset, subCmd)
		fmt.Fprintf(stderr, "%sRun %staskview server help%s for usage.%s\n", c.Dim, c.Reset, c.Dim, c.Reset)
		return 1
	}

	for k := range flags {
		if !allowed[k] && !allowed[camelizeKey(k)] {
			if len(k) == 1 {
				fmt.Fprintf(stderr, "%sError:%s unknown option \"-%s\"\n", c.Red, c.Reset, k)
			} else {
				fmt.Fprintf(stderr, "%sError:%s unknown option \"--%s\"\n", c.Red, c.Reset, k)
			}
			return 1
		}
	}

	switch subCmd {
	case "start":
		return cmdServerStart(stdout, stderr, flags, args, c)
	case "stop":
		return cmdServerStop(stdout, stderr, args, c)
	case "status":
		return cmdServerStatus(stdout, stderr, args, c)
	case "run":
		return cmdServerRun(stdout, stderr, flags, args, c)
	default:
		fmt.Fprintf(stderr, "%sError:%s unknown server command \"%s\"\n", c.Red, c.Reset, subCmd)
		return 1
	}
}

func resolveServerDataDir(flags map[string]string) (string, error) {
	if !hasFlag(flags, "data-dir", "dataDir") {
		return "", nil
	}
	dir := getFlag(flags, "data-dir", "dataDir")
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("data directory cannot be empty")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("invalid data directory: %w", err)
	}
	return absDir, nil
}

func cmdServerStart(stdout, stderr io.Writer, flags map[string]string, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	port := 3333
	if hasFlag(flags, "port", "p") {
		p, err := parsePort(getFlag(flags, "port", "p"))
		if err != nil {
			fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
			return 1
		}
		port = p
	}

	absDataDir, err := resolveServerDataDir(flags)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	if absDataDir != "" {
		if err := os.Setenv("TASKVIEW_HOME", absDataDir); err != nil {
			fmt.Fprintf(stderr, "%sError:%s failed to set TASKVIEW_HOME: %s\n", c.Red, c.Reset, err.Error())
			return 1
		}
	}

	state, err := lifecycleEnsureBackgroundServer(port)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	dataDir, err := DataDir()
	logPath := "server.log"
	if err == nil {
		if absDataDir, err := filepath.Abs(dataDir); err == nil {
			logPath = filepath.Join(absDataDir, "server.log")
		} else {
			logPath = filepath.Join(dataDir, "server.log")
		}
	}

	fmt.Fprintf(stdout, "TaskView server running at %s (PID %d)\n", state.URL, state.PID)
	fmt.Fprintf(stdout, "Log path: %s\n", logPath)
	return 0
}

func cmdServerStop(stdout, stderr io.Writer, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	if err := lifecycleStopBackgroundServer(); err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	fmt.Fprintf(stdout, "TaskView server stopped.\n")
	return 0
}

func cmdServerStatus(stdout, stderr io.Writer, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	state, err := lifecycleManagedServerStatus()
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	if state == nil {
		fmt.Fprintln(stdout, "TaskView server is not running.")
		return 0
	}

	fmt.Fprintf(stdout, "TaskView server is running at %s (PID %d)\n", state.URL, state.PID)
	if state.DB != "" {
		fmt.Fprintf(stdout, "Database: %s\n", state.DB)
	}
	if state.StartedAt > 0 {
		started := time.UnixMilli(state.StartedAt).UTC().Format(time.RFC3339)
		fmt.Fprintf(stdout, "Started:  %s\n", started)
	}
	return 0
}

func cmdServerRun(stdout, stderr io.Writer, flags map[string]string, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	port := 3333
	if hasFlag(flags, "port", "p") {
		p, err := parsePort(getFlag(flags, "port", "p"))
		if err != nil {
			fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
			return 1
		}
		port = p
	}

	absDataDir, err := resolveServerDataDir(flags)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	if absDataDir != "" {
		if err := os.Setenv("TASKVIEW_HOME", absDataDir); err != nil {
			fmt.Fprintf(stderr, "%sError:%s failed to set TASKVIEW_HOME: %s\n", c.Red, c.Reset, err.Error())
			return 1
		}
	}

	fmt.Fprintf(stdout, "Starting TaskView server on port %d...\n", port)

	if err := lifecycleRunManagedServer(port); err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}
	return 0
}

func cmdUI(stdout, stderr io.Writer, flags map[string]string, boolFlags map[string]bool, positional []string, c ColorConfig) int {
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "%sError:%s unexpected argument \"%s\"\n", c.Red, c.Reset, positional[0])
		return 1
	}

	port := 3333
	if hasFlag(flags, "port", "p") {
		p, err := parsePort(getFlag(flags, "port", "p"))
		if err != nil {
			fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
			return 1
		}
		port = p
	}

	noOpen := getBoolFlag(boolFlags, "no-open", "noOpen") || getFlag(flags, "open") == "false"

	state, err := lifecycleEnsureBackgroundServer(port)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s failed to start server: %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	if !noOpen {
		lifecycleOpenBrowser(state.URL)
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  %sTaskView%s running at %s%s%s\n", c.Green, c.Reset, c.Cyan, state.URL, c.Reset)
	if state.DB != "" {
		fmt.Fprintf(stdout, "  %sdatabase: %s%s\n", c.Dim, state.DB, c.Reset)
	}
	fmt.Fprintln(stdout)
	return 0
}

func isTaskCommand(cmd string) bool {
	switch cmd {
	case "add", "list", "get", "update", "delete", "stats":
		return true
	default:
		return false
	}
}

// RunCLI executes command line parsing and dispatch.
func RunCLI(argv []string, store *Store, stdout, stderr io.Writer) int {
	c := initColors(stdout)

	parsed, err := parseCLIArgs(argv)
	if err != nil {
		fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
		return 1
	}

	if getBoolFlag(parsed.BoolFlags, "version", "v") || (len(parsed.Positional) > 0 && parsed.Positional[0] == "version") {
		fmt.Fprintln(stdout, "taskview 1.0.0")
		return 0
	}

	if len(parsed.Positional) == 0 || getBoolFlag(parsed.BoolFlags, "help", "h") || parsed.Positional[0] == "help" {
		fmt.Fprint(stdout, getHelp(c))
		return 0
	}

	rawCmd := parsed.Positional[0]
	cmd := canonicalCommand(rawCmd)
	args := parsed.Positional[1:]

	if cmd != "server" {
		if err := validateFlagsForCommand(cmd, parsed.Flags); err != nil {
			fmt.Fprintf(stderr, "%sError:%s %s\n", c.Red, c.Reset, err.Error())
			return 1
		}
	}

	// Service/management commands do not need a Store.
	switch cmd {
	case "server":
		return cmdServer(stdout, stderr, parsed.Flags, parsed.BoolFlags, args, c)
	case "install":
		return cmdInstall(stdout, stderr, parsed.Flags, parsed.BoolFlags, args, c)
	case "uninstall":
		return cmdUninstall(stdout, stderr, args, c)
	case "ui":
		return cmdUI(stdout, stderr, parsed.Flags, parsed.BoolFlags, args, c)
	}

	// For task commands, ensure store is open
	if isTaskCommand(cmd) && store == nil {
		var err error
		store, err = OpenStore("")
		if err != nil {
			fmt.Fprintf(stderr, "%sError:%s failed to open database: %v\n", c.Red, c.Reset, err)
			return 1
		}
		defer store.Close()
	}

	switch cmd {
	case "add":
		return cmdAdd(stdout, stderr, store, parsed.Flags, args, c)
	case "list":
		return cmdList(stdout, stderr, store, parsed.Flags, parsed.BoolFlags, c)
	case "get":
		return cmdGet(stdout, stderr, store, args, c)
	case "update":
		return cmdUpdate(stdout, stderr, store, parsed.Flags, args, c)
	case "delete":
		return cmdDelete(stdout, stderr, store, args, c)
	case "stats":
		return cmdStats(stdout, stderr, store, parsed.Flags, parsed.BoolFlags, c)
	default:
		fmt.Fprintf(stderr, "%sError:%s unknown command \"%s\"\n", c.Red, c.Reset, rawCmd)
		fmt.Fprintf(stderr, "%sRun %staskview help%s for usage.%s\n", c.Dim, c.Reset, c.Dim, c.Reset)
		return 1
	}
}
