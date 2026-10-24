package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"taskview/internal/db"
)

func boardSendJSON(w http.ResponseWriter, status int, payload any) {
	SendJSON(w, status, payload)
}

func boardSendError(w http.ResponseWriter, status int, msg string) {
	SendError(w, status, msg)
}

func boardReadJSONBody(r *http.Request) (map[string]any, error) {
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, &db.ValidationError{Message: "Failed to read request body"}
	}
	if len(data) > maxBodyBytes {
		return nil, &db.ValidationError{Message: "Request body too large"}
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, &db.ValidationError{Message: "Request body must be valid JSON"}
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, &db.ValidationError{Message: "Request body must be a JSON object"}
	}
	return obj, nil
}

func boardParseListPaging(q url.Values) (limit, offset int, err error) {
	limit = 500
	offset = 0

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return 0, 0, db.NewValidationError("Invalid limit %q: expected an integer", raw)
		}
		if parsed < 1 || parsed > 500 {
			return 0, 0, db.NewValidationError("Invalid limit %d: must be between 1 and 500", parsed)
		}
		limit = parsed
	}

	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return 0, 0, db.NewValidationError("Invalid offset %q: expected an integer", raw)
		}
		if parsed < 0 {
			return 0, 0, db.NewValidationError("Invalid offset %d: must not be negative", parsed)
		}
		offset = parsed
	}

	return limit, offset, nil
}

func boardPrepareCreatePayload(body map[string]any) (map[string]any, error) {
	canonical := db.Canonicalize(body)

	raw, ok := canonical["project_path"]
	if !ok {
		return nil, db.NewValidationError("project_path is required and must be an absolute path")
	}
	rawPath, _, err := db.RequireString(raw, "project_path")
	if err != nil {
		return nil, err
	}
	projectPath, err := db.NormalizeProjectPath(rawPath)
	if err != nil {
		return nil, err
	}

	canonical["project_path"] = projectPath
	canonical["project_name"] = db.ProjectNameFromPath(projectPath)
	return canonical, nil
}

var boardWritableTaskColumns = map[string]bool{
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

func boardPickWritable(body map[string]any) map[string]any {
	out := make(map[string]any)
	for key, value := range db.Canonicalize(body) {
		if boardWritableTaskColumns[key] {
			out[key] = value
		}
	}
	return out
}

// boardTaskListItem is the list representation of a task.
//
// The board list may hold hundreds of tasks and is fetched page by page, so it
// carries neither the code snippet nor the agent notes body. The single-task
// endpoint (GET /api/board/tasks/{id}) still returns the full Task, and so does
// the CLI. has_agent_notes keeps the list's “包含智能体备注” marker without
// shipping the notes themselves.
type boardTaskListItem struct {
	ID              string  `json:"id"`
	ProjectPath     string  `json:"project_path"`
	ProjectName     string  `json:"project_name"`
	Title           string  `json:"title"`
	Status          string  `json:"status"`
	Priority        string  `json:"priority"`
	Description     *string `json:"description"`
	FilePath        *string `json:"file_path"`
	LineStart       *int64  `json:"line_start"`
	LineEnd         *int64  `json:"line_end"`
	RejectionReason *string `json:"rejection_reason"`
	CreatedAt       int64   `json:"created_at"`
	UpdatedAt       int64   `json:"updated_at"`
	ClosedAt        *int64  `json:"closed_at"`
	HasAgentNotes   bool    `json:"has_agent_notes"`
}

// boardTaskList projects a full task onto the list representation. The body
// fields are dropped; every other field keeps its name and value.
func boardTaskList(tasks []db.Task) []boardTaskListItem {
	items := make([]boardTaskListItem, len(tasks))
	for i, task := range tasks {
		items[i] = boardTaskListItem{
			ID:              task.ID,
			ProjectPath:     task.ProjectPath,
			ProjectName:     task.ProjectName,
			Title:           task.Title,
			Status:          task.Status,
			Priority:        task.Priority,
			Description:     task.Description,
			FilePath:        task.FilePath,
			LineStart:       task.LineStart,
			LineEnd:         task.LineEnd,
			RejectionReason: task.RejectionReason,
			CreatedAt:       task.CreatedAt,
			UpdatedAt:       task.UpdatedAt,
			ClosedAt:        task.ClosedAt,
			HasAgentNotes:   task.AgentNotes != nil && *task.AgentNotes != "",
		}
	}
	return items
}

func handleBoardAPI(store *db.Store, w http.ResponseWriter, r *http.Request, cleanPath string) {
	relPath := strings.TrimPrefix(cleanPath, "/api/board")
	if relPath == "" {
		relPath = "/"
	}

	// GET /api/board/projects
	if relPath == "/projects" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			boardSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
			return
		}
		projects, err := store.GetProjects()
		if err != nil {
			boardSendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if projects == nil {
			projects = []db.Project{}
		}
		boardSendJSON(w, http.StatusOK, projects)
		return
	}

	// GET /api/board/stats
	if relPath == "/stats" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			boardSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
			return
		}
		q := r.URL.Query()
		scope := q.Get("project_path")
		if scope == "" {
			scope = q.Get("projectPath")
		}
		if trimmed := strings.TrimSpace(scope); trimmed != "" && trimmed != "all" {
			normalized, err := db.NormalizeProjectPath(trimmed)
			if err != nil {
				boardSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			scope = normalized
		}
		stats, err := store.GetStats(scope)
		if err != nil {
			boardSendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		boardSendJSON(w, http.StatusOK, stats)
		return
	}

	// /api/board/tasks
	if relPath == "/tasks" {
		if r.Method == http.MethodGet {
			q := r.URL.Query()
			if len(q["status"]) > 1 {
				boardSendError(w, http.StatusBadRequest, "status filter only allows a single value")
				return
			}
			if len(q["priority"]) > 1 {
				boardSendError(w, http.StatusBadRequest, "priority filter only allows a single value")
				return
			}
			allBool := false
			if b, ok := ParseBoolParam(q.Get("all")); ok && b {
				allBool = true
			}
			search := q.Get("search")
			if search == "" {
				search = q.Get("q")
			}
			projParam := q.Get("project_path")
			if projParam == "" {
				projParam = q.Get("projectPath")
			}
			opts := db.ListOptions{
				Status:   q.Get("status"),
				Priority: q.Get("priority"),
				Search:   search,
				Sort:     q.Get("sort"),
				All:      allBool,
			}
			if !allBool {
				scope, err := db.NormalizeProjectPath(projParam)
				if err != nil {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				opts.ProjectPath = scope
			}
			limit, offset, err := boardParseListPaging(q)
			if err != nil {
				boardSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			opts.Limit = limit
			opts.Offset = offset
			tasks, err := store.ListTasks(opts)
			if err != nil {
				if db.IsValidationError(err) {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tasks == nil {
				tasks = []db.Task{}
			}
			// The list is the hot path: drop the two large text bodies and send
			// only the boolean the list marker needs.
			boardSendJSON(w, http.StatusOK, boardTaskList(tasks))
			return
		}

		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			body, err := boardReadJSONBody(r)
			if err != nil {
				boardSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			data, err := boardPrepareCreatePayload(body)
			if err != nil {
				boardSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			task, err := store.CreateTask(data)
			if err != nil {
				if db.IsValidationError(err) {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			boardSendJSON(w, http.StatusCreated, task)
			return
		}

		w.Header().Set("Allow", "GET, POST, PUT")
		boardSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
		return
	}

	// /api/board/tasks/...
	if strings.HasPrefix(relPath, "/tasks/") {
		idPart := strings.TrimPrefix(relPath, "/tasks/")
		sub := ""
		if slash := strings.Index(idPart, "/"); slash >= 0 {
			idPart, sub = idPart[:slash], idPart[slash+1:]
		}
		id, unescapeErr := url.PathUnescape(idPart)
		if unescapeErr != nil || id == "" || strings.Contains(id, "/") {
			boardSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", idPart))
			return
		}

		// POST /api/board/tasks/{id}/reject
		if sub == "reject" {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				boardSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
				return
			}
			body, err := boardReadJSONBody(r)
			if err != nil {
				boardSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			reason, ok := body["reason"].(string)
			if !ok {
				boardSendError(w, http.StatusBadRequest, "reason is required and must be a string")
				return
			}
			reason = strings.TrimSpace(reason)
			if reason == "" {
				boardSendError(w, http.StatusBadRequest, "reason must not be empty")
				return
			}

			updated, err := store.RejectTask(id, reason)
			if err != nil {
				if db.IsValidationError(err) {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if updated == nil {
				boardSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}
			boardSendJSON(w, http.StatusOK, updated)
			return
		}

		if sub != "" {
			boardSendError(w, http.StatusNotFound, fmt.Sprintf("Unknown API endpoint: %s", cleanPath))
			return
		}

		// GET /api/board/tasks/{id}
		if r.Method == http.MethodGet {
			task, err := store.GetTask(id)
			if err != nil {
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if task == nil {
				boardSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}
			boardSendJSON(w, http.StatusOK, task)
			return
		}

		// PATCH /api/board/tasks/{id}
		if r.Method == http.MethodPatch || r.Method == http.MethodPut {
			body, err := boardReadJSONBody(r)
			if err != nil {
				boardSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields := boardPickWritable(body)
			if len(fields) == 0 {
				boardSendError(w, http.StatusBadRequest, "No valid fields provided to update")
				return
			}
			if raw, ok := fields["project_path"]; ok {
				rawPath, _, err := db.RequireString(raw, "project_path")
				if err != nil {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				normalized, err := db.NormalizeProjectPath(rawPath)
				if err != nil {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				fields["project_path"] = normalized
			}

			existing, err := store.GetTask(id)
			if err != nil {
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if existing == nil {
				boardSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}

			updated, err := store.UpdateTask(id, fields)
			if err != nil {
				if db.IsValidationError(err) {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			boardSendJSON(w, http.StatusOK, updated)
			return
		}

		// DELETE /api/board/tasks/{id}
		if r.Method == http.MethodDelete {
			deleted, err := store.DeleteTask(id)
			if err != nil {
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !deleted {
				boardSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}
			boardSendJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
			return
		}

		w.Header().Set("Allow", "GET, PATCH, PUT, DELETE")
		boardSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
		return
	}

	boardSendError(w, http.StatusNotFound, fmt.Sprintf("Unknown Board API endpoint: %s", cleanPath))
}
