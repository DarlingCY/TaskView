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

func agentSendJSON(w http.ResponseWriter, status int, payload any) {
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

func agentSendError(w http.ResponseWriter, status int, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "Error"
		}
	}
	agentSendJSON(w, status, map[string]any{
		"error":   msg,
		"success": false,
	})
}

func agentReadJSONBody(r *http.Request) (map[string]any, error) {
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

func agentParseListPaging(q url.Values) (limit, offset int, err error) {
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

func agentPrepareCreatePayload(body map[string]any) (map[string]any, error) {
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

var agentWritableTaskColumns = map[string]bool{
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

func agentPickWritable(body map[string]any) map[string]any {
	out := make(map[string]any)
	for key, value := range canonicalize(body) {
		if agentWritableTaskColumns[key] {
			out[key] = value
		}
	}
	return out
}

func handleAgentAPI(store *Store, w http.ResponseWriter, r *http.Request, cleanPath string) {
	relPath := strings.TrimPrefix(cleanPath, "/api/agent")
	if relPath == "" {
		relPath = "/"
	}

	// GET /api/agent/health
	if relPath == "/health" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			agentSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
			return
		}
		dbPath := ""
		if store != nil {
			dbPath = store.FilePath
		}
		payload := map[string]any{
			"success": true,
			"version": appVersion(),
			"db":      dbPath,
		}
		if store == nil {
			payload["success"] = false
			payload["error"] = "database is not available"
			agentSendJSON(w, http.StatusServiceUnavailable, payload)
			return
		}
		if err := store.Ping(); err != nil {
			payload["success"] = false
			payload["error"] = "database is not readable: " + err.Error()
			agentSendJSON(w, http.StatusServiceUnavailable, payload)
			return
		}
		agentSendJSON(w, http.StatusOK, payload)
		return
	}

	// /api/agent/tasks
	if relPath == "/tasks" {
		if r.Method == http.MethodGet {
			q := r.URL.Query()
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
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				opts.ProjectPath = scope
			}
			limit, offset, err := agentParseListPaging(q)
			if err != nil {
				agentSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			opts.Limit = limit
			opts.Offset = offset
			tasks, err := store.ListTasks(opts)
			if err != nil {
				if IsValidationError(err) {
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				agentSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tasks == nil {
				tasks = []Task{}
			}
			agentSendJSON(w, http.StatusOK, tasks)
			return
		}

		if r.Method == http.MethodPost {
			body, err := agentReadJSONBody(r)
			if err != nil {
				agentSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			data, err := agentPrepareCreatePayload(body)
			if err != nil {
				agentSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			task, err := store.CreateTask(data)
			if err != nil {
				if IsValidationError(err) {
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				agentSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			agentSendJSON(w, http.StatusCreated, task)
			return
		}

		w.Header().Set("Allow", "GET, POST")
		agentSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
		return
	}

	// /api/agent/tasks/...
	if strings.HasPrefix(relPath, "/tasks/") {
		idPart := strings.TrimPrefix(relPath, "/tasks/")
		sub := ""
		if slash := strings.Index(idPart, "/"); slash >= 0 {
			idPart, sub = idPart[:slash], idPart[slash+1:]
		}
		id, unescapeErr := url.PathUnescape(idPart)
		if unescapeErr != nil || id == "" || strings.Contains(id, "/") {
			agentSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", idPart))
			return
		}

		// POST /api/agent/tasks/{id}/notes
		if sub == "notes" {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				agentSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
				return
			}
			body, err := agentReadJSONBody(r)
			if err != nil {
				agentSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			note, ok := body["note"].(string)
			if !ok {
				agentSendError(w, http.StatusBadRequest, "note is required and must be a string")
				return
			}
			note = strings.TrimSpace(note)
			if note == "" {
				agentSendError(w, http.StatusBadRequest, "note must not be empty")
				return
			}

			updated, err := store.AppendNotes(id, note)
			if err != nil {
				if IsValidationError(err) {
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				agentSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if updated == nil {
				agentSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}
			agentSendJSON(w, http.StatusOK, updated)
			return
		}

		if sub != "" {
			agentSendError(w, http.StatusNotFound, fmt.Sprintf("Unknown API endpoint: %s", cleanPath))
			return
		}

		// GET /api/agent/tasks/{id}
		if r.Method == http.MethodGet {
			task, err := store.GetTask(id)
			if err != nil {
				agentSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if task == nil {
				agentSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}
			agentSendJSON(w, http.StatusOK, task)
			return
		}

		// PATCH /api/agent/tasks/{id}
		if r.Method == http.MethodPatch || r.Method == http.MethodPut {
			body, err := agentReadJSONBody(r)
			if err != nil {
				agentSendError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields := agentPickWritable(body)
			if len(fields) == 0 {
				agentSendError(w, http.StatusBadRequest, "No valid fields provided to update")
				return
			}
			if raw, ok := fields["project_path"]; ok {
				rawPath, _, err := requireString(raw, "project_path")
				if err != nil {
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				normalized, err := NormalizeProjectPath(rawPath)
				if err != nil {
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				fields["project_path"] = normalized
			}

			existing, err := store.GetTask(id)
			if err != nil {
				agentSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if existing == nil {
				agentSendError(w, http.StatusNotFound, fmt.Sprintf("Task not found: %s", id))
				return
			}

			updated, err := store.UpdateTask(id, fields)
			if err != nil {
				if IsValidationError(err) {
					agentSendError(w, http.StatusBadRequest, err.Error())
					return
				}
				agentSendError(w, http.StatusInternalServerError, err.Error())
				return
			}
			agentSendJSON(w, http.StatusOK, updated)
			return
		}

		w.Header().Set("Allow", "GET, PATCH, PUT")
		agentSendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method %s not allowed", r.Method))
		return
	}

	agentSendError(w, http.StatusNotFound, fmt.Sprintf("Unknown Agent API endpoint: %s", cleanPath))
}
