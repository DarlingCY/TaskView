package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func boardSendJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"Internal server error","success":false}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func boardSendError(w http.ResponseWriter, status int, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "Error"
		}
	}
	boardSendJSON(w, status, map[string]any{
		"error":   msg,
		"success": false,
	})
}

func boardReadJSONBody(r *http.Request) (map[string]any, error) {
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, &ValidationError{Message: "Failed to read request body"}
	}
	if len(data) > maxBodyBytes {
		return nil, &ValidationError{Message: "Request body too large"}
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, &ValidationError{Message: "Request body must be valid JSON"}
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, &ValidationError{Message: "Request body must be a JSON object"}
	}
	return obj, nil
}

func boardParseListPaging(q url.Values) (limit, offset int, err error) {
	limit = 500
	offset = 0

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return 0, 0, newValidationError("Invalid limit %q: expected an integer", raw)
		}
		if parsed < 1 || parsed > 500 {
			return 0, 0, newValidationError("Invalid limit %d: must be between 1 and 500", parsed)
		}
		limit = parsed
	}

	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return 0, 0, newValidationError("Invalid offset %q: expected an integer", raw)
		}
		if parsed < 0 {
			return 0, 0, newValidationError("Invalid offset %d: must not be negative", parsed)
		}
		offset = parsed
	}

	return limit, offset, nil
}

func boardPrepareCreatePayload(body map[string]any) (map[string]any, error) {
	canonical := canonicalize(body)

	raw, ok := canonical["project_path"]
	if !ok {
		return nil, newValidationError("project_path is required and must be an absolute path")
	}
	rawPath, _, err := requireString(raw, "project_path")
	if err != nil {
		return nil, err
	}
	projectPath, err := NormalizeProjectPath(rawPath)
	if err != nil {
		return nil, err
	}

	canonical["project_path"] = projectPath
	canonical["project_name"] = ProjectNameFromPath(projectPath)
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
	for key, value := range canonicalize(body) {
		if boardWritableTaskColumns[key] {
			out[key] = value
		}
	}
	return out
}

func handleBoardAPI(store *Store, w http.ResponseWriter, r *http.Request, cleanPath string) {
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
			projects = []Project{}
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
			normalized, err := NormalizeProjectPath(trimmed)
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
			if b, ok := parseBoolParam(q.Get("all")); ok && b {
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
			opts := ListOptions{
				Status:   q.Get("status"),
				Priority: q.Get("priority"),
				Search:   search,
				Sort:     q.Get("sort"),
				All:      allBool,
			}
			if !allBool {
				scope, err := NormalizeProjectPath(projParam)
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
				if IsValidationError(err) {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				boardSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tasks == nil {
				tasks = []Task{}
			}
			boardSendJSON(w, http.StatusOK, tasks)
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
				if IsValidationError(err) {
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
		id, unescapeErr := url.PathUnescape(idPart)
		if unescapeErr != nil || id == "" || strings.Contains(id, "/") {
			boardSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", idPart))
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
				rawPath, _, err := requireString(raw, "project_path")
				if err != nil {
					boardSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				normalized, err := NormalizeProjectPath(rawPath)
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
				if IsValidationError(err) {
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
