package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func newTestServerStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "server_test.db"))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func testStaticFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {
			Data: []byte("<!doctype html><title>TaskView</title>"),
		},
		"app.js": {
			Data: []byte("console.log('ui');"),
		},
		"style.css": {
			Data: []byte("body { margin: 0; }"),
		},
	}
}

func testReq(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, body)
	req.Host = "127.0.0.1"
	return req
}

func TestStaticServingAndHEAD(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	t.Run("serves index.html at /", func(t *testing.T) {
		req := testReq(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("expected text/html, got %s", rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), "TaskView") {
			t.Fatalf("body does not contain TaskView: %s", rec.Body.String())
		}
	})

	t.Run("serves js and css with correct headers", func(t *testing.T) {
		reqJS := testReq(http.MethodGet, "/app.js", nil)
		recJS := httptest.NewRecorder()
		handler.ServeHTTP(recJS, reqJS)
		if recJS.Code != http.StatusOK {
			t.Fatalf("expected 200 for app.js, got %d", recJS.Code)
		}
		if !strings.Contains(recJS.Header().Get("Content-Type"), "text/javascript") {
			t.Fatalf("expected text/javascript, got %s", recJS.Header().Get("Content-Type"))
		}
		if recJS.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("expected no-cache, got %s", recJS.Header().Get("Cache-Control"))
		}

		reqCSS := testReq(http.MethodGet, "/style.css", nil)
		recCSS := httptest.NewRecorder()
		handler.ServeHTTP(recCSS, reqCSS)
		if recCSS.Code != http.StatusOK {
			t.Fatalf("expected 200 for style.css, got %d", recCSS.Code)
		}
		if !strings.Contains(recCSS.Header().Get("Content-Type"), "text/css") {
			t.Fatalf("expected text/css, got %s", recCSS.Header().Get("Content-Type"))
		}
	})

	t.Run("HEAD request returns headers without body", func(t *testing.T) {
		req := testReq(http.MethodHead, "/style.css", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 on HEAD, got %d", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("expected empty body on HEAD, got %d bytes", rec.Body.Len())
		}
		if rec.Header().Get("Content-Length") == "" {
			t.Fatalf("expected Content-Length header on HEAD")
		}
	})

	t.Run("unknown static path returns 404 JSON", func(t *testing.T) {
		req := testReq(http.MethodGet, "/does-not-exist.png", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("expected json error response, got %s", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("directory traversal blocked", func(t *testing.T) {
		req := testReq(http.MethodGet, "/../../../etc/passwd", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
			t.Fatalf("expected 404 or 403 on traversal, got %d", rec.Code)
		}
	})

	t.Run("unsupported method on static returns 405", func(t *testing.T) {
		req := testReq(http.MethodPost, "/app.js", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Allow"), "GET") {
			t.Fatalf("expected Allow header, got %s", rec.Header().Get("Allow"))
		}
	})
}

func TestSecurityAndHostOrigin(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	cases := []struct {
		name      string
		host      string
		origin    string
		wantCode  int
		wantAllow bool
	}{
		{"same-origin exact match", "127.0.0.1:3333", "http://127.0.0.1:3333", http.StatusOK, true},
		{"same-origin localhost match", "localhost:3333", "http://localhost:3333", http.StatusOK, true},
		{"https scheme rejected", "127.0.0.1:3333", "https://127.0.0.1:3333", http.StatusForbidden, false},
		{"origin with trailing slash path rejected", "127.0.0.1:3333", "http://127.0.0.1:3333/", http.StatusForbidden, false},
		{"origin with subpath rejected", "127.0.0.1:3333", "http://127.0.0.1:3333/api", http.StatusForbidden, false},
		{"origin with query rejected", "127.0.0.1:3333", "http://127.0.0.1:3333?q=1", http.StatusForbidden, false},
		{"origin with fragment rejected", "127.0.0.1:3333", "http://127.0.0.1:3333#frag", http.StatusForbidden, false},
		{"local cross-port rejected", "127.0.0.1:3333", "http://127.0.0.1:4000", http.StatusForbidden, false},
		{"local hostname mismatch rejected", "127.0.0.1:3333", "http://localhost:3333", http.StatusForbidden, false},
		{"foreign origin rejected", "127.0.0.1:3333", "https://malicious-website.com", http.StatusForbidden, false},
		{"null origin rejected", "127.0.0.1:3333", "null", http.StatusForbidden, false},
		{"origin with credentials rejected", "127.0.0.1:3333", "http://user:pass@127.0.0.1:3333", http.StatusForbidden, false},
		{"non-loopback host rejected", "attacker.dnsrebinding.com:3333", "http://attacker.dnsrebinding.com:3333", http.StatusBadRequest, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/api/health", nil)
			req.Host = tc.host
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected status %d, got %d (body: %s)", tc.wantCode, rec.Code, rec.Body.String())
			}
			allowOrigin := rec.Header().Get("Access-Control-Allow-Origin")
			if tc.wantAllow && allowOrigin != tc.origin {
				t.Fatalf("expected Access-Control-Allow-Origin %q, got %q", tc.origin, allowOrigin)
			}
			if !tc.wantAllow && allowOrigin != "" {
				t.Fatalf("expected no Access-Control-Allow-Origin on rejected request, got %q", allowOrigin)
			}
		})
	}

	t.Run("OPTIONS preflight returns 204 for same-origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:3333/api/board/tasks", nil)
		req.Host = "127.0.0.1:3333"
		req.Header.Set("Origin", "http://127.0.0.1:3333")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PATCH") {
			t.Fatalf("missing allow methods: %s", rec.Header().Get("Access-Control-Allow-Methods"))
		}
	})
}

func TestAPIEndpoints(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	t.Run("GET /api and /api/health and /api/agent/health", func(t *testing.T) {
		for _, endpoint := range []string{"/api", "/api/health", "/api/agent/health"} {
			req := testReq(http.MethodGet, endpoint, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s: expected 200, got %d", endpoint, rec.Code)
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("failed to parse json: %v", err)
			}
			if res["success"] != true || res["db"] != store.FilePath {
				t.Fatalf("unexpected response: %v", res)
			}
			if res["version"] != appVersion() {
				t.Fatalf("version = %v, want %q", res["version"], appVersion())
			}
		}
	})

	t.Run("GET /api/board/projects returns [] on empty store", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/projects", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("expected exact '[]' for empty projects, got: %q", rec.Body.String())
		}
	})

	t.Run("GET /api/board/stats", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/stats", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var stats map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
			t.Fatalf("failed to parse stats: %v", err)
		}
		for _, key := range []string{"total", "backlog", "todo", "in_progress", "done", "open"} {
			if _, ok := stats[key]; !ok {
				t.Fatalf("missing key %s in stats", key)
			}
		}
	})

	t.Run("GET unknown API endpoint", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/not-an-endpoint", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
}

func TestBoardTasksCRUD(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	// The project has to be named explicitly: the server's working directory
	// is the install directory, never the user's project.
	projectPath := filepath.Join(t.TempDir(), "my-app")
	var createdID string

	t.Run("GET /api/board/tasks returns [] on empty store", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("expected exact '[]' for empty tasks, got: %q", rec.Body.String())
		}
	})

	t.Run("GET /api/board/tasks without a scope is rejected", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("POST /api/board/tasks creates task", func(t *testing.T) {
		body := fmt.Sprintf(`{"title":"HTTP Test Task","priority":"high","status":"todo","project_path":%q}`, projectPath)
		req := testReq(http.MethodPost, "/api/board/tasks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}

		var task map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if task["title"] != "HTTP Test Task" || task["priority"] != "high" {
			t.Fatalf("task fields mismatch: %v", task)
		}
		// Verify camelCase alias
		if task["projectPath"] == nil || task["createdAt"] == nil {
			t.Fatalf("missing camelCase aliases in json: %v", task)
		}
		if task["project_path"] != projectPath {
			t.Fatalf("project_path = %v, want %q", task["project_path"], projectPath)
		}
		if task["project_name"] != "my-app" {
			t.Fatalf("project_name = %v, want the last directory of the path", task["project_name"])
		}

		id, ok := task["id"].(string)
		if !ok || id == "" {
			t.Fatalf("missing or invalid id: %v", task["id"])
		}
		createdID = id
	})

	t.Run("PUT /api/board/tasks creates task (compatibility)", func(t *testing.T) {
		body := fmt.Sprintf(`{"title":"PUT Created Task","project_path":%q}`, projectPath)
		req := testReq(http.MethodPut, "/api/board/tasks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GET /api/board/tasks/:id retrieves task", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks/"+createdID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var task map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("failed to parse task: %v", err)
		}
		if task["id"] != createdID {
			t.Fatalf("expected id %s, got %v", createdID, task["id"])
		}
	})

	t.Run("GET /api/board/tasks/unknown returns 404", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks/task_does_not_exist", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("PATCH /api/board/tasks/:id updates task", func(t *testing.T) {
		body := `{"status":"in_progress","agentNotes":"working on api test"}`
		req := testReq(http.MethodPatch, "/api/board/tasks/"+createdID, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var task map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("failed to parse updated task: %v", err)
		}
		if task["status"] != "in_progress" || task["agent_notes"] != "working on api test" {
			t.Fatalf("task fields not updated: %v", task)
		}
	})

	t.Run("PATCH /api/board/tasks/:id SPA full edit payload clearing nullable fields", func(t *testing.T) {
		body := fmt.Sprintf(`{
			"title": "SPA Cleared Task",
			"description": null,
			"filePath": null,
			"lineStart": null,
			"lineEnd": null,
			"codeSnippet": null,
			"agentNotes": null,
			"projectPath": %q,
			"projectName": "my-app"
		}`, projectPath)
		req := testReq(http.MethodPatch, "/api/board/tasks/"+createdID, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var task map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("failed to parse updated task: %v", err)
		}
		if task["title"] != "SPA Cleared Task" {
			t.Fatalf("expected title 'SPA Cleared Task', got %v", task["title"])
		}

		dbTask, err := store.GetTask(createdID)
		if err != nil || dbTask == nil {
			t.Fatalf("failed to retrieve task from store: %v", err)
		}
		if dbTask.Description != nil || dbTask.FilePath != nil || dbTask.LineStart != nil || dbTask.LineEnd != nil || dbTask.CodeSnippet != nil || dbTask.AgentNotes != nil {
			t.Fatalf("expected all nullable fields to be cleared, got: %+v", dbTask)
		}
	})

	t.Run("PATCH /api/board/tasks/:id with no valid fields returns 400", func(t *testing.T) {
		body := `{"unrecognizedField":"abc"}`
		req := testReq(http.MethodPatch, "/api/board/tasks/"+createdID, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("DELETE /api/board/tasks/:id deletes task", func(t *testing.T) {
		req := testReq(http.MethodDelete, "/api/board/tasks/"+createdID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to parse delete response: %v", err)
		}
		if res["success"] != true {
			t.Fatalf("expected success true, got %v", res)
		}

		// Subsequent GET returns 404
		reqGet := testReq(http.MethodGet, "/api/board/tasks/"+createdID, nil)
		recGet := httptest.NewRecorder()
		handler.ServeHTTP(recGet, reqGet)
		if recGet.Code != http.StatusNotFound {
			t.Fatalf("expected 404 after delete, got %d", recGet.Code)
		}
	})

	t.Run("DELETE nonexistent task returns 404", func(t *testing.T) {
		req := testReq(http.MethodDelete, "/api/board/tasks/task_already_deleted", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("unsupported method on /api/board/tasks/:id returns 405", func(t *testing.T) {
		req := testReq(http.MethodPost, "/api/board/tasks/task_123", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Allow"), "GET") {
			t.Fatalf("expected Allow header, got %s", rec.Header().Get("Allow"))
		}
	})
}

func TestAgentTasksCRUD(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	projectPath := filepath.Join(t.TempDir(), "my-app")
	var createdID string

	t.Run("GET /api/agent/tasks returns [] on empty store", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/agent/tasks?all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("expected exact '[]', got: %q", rec.Body.String())
		}
	})

	t.Run("POST /api/agent/tasks creates task", func(t *testing.T) {
		body := fmt.Sprintf(`{"title":"Agent Test Task","priority":"high","status":"todo","project_path":%q}`, projectPath)
		req := testReq(http.MethodPost, "/api/agent/tasks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}

		var task map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if task["title"] != "Agent Test Task" || task["priority"] != "high" {
			t.Fatalf("task fields mismatch: %v", task)
		}
		createdID = task["id"].(string)
	})

	t.Run("GET /api/agent/tasks/:id retrieves task", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/agent/tasks/"+createdID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("PATCH /api/agent/tasks/:id updates task", func(t *testing.T) {
		body := `{"status":"in_progress"}`
		req := testReq(http.MethodPatch, "/api/agent/tasks/"+createdID, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("POST /api/agent/tasks/:id/notes appends note", func(t *testing.T) {
		body := `{"note":"first note"}`
		req := testReq(http.MethodPost, "/api/agent/tasks/"+createdID+"/notes", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var task map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("failed to parse: %v", err)
		}
		if task["agent_notes"] != "first note" {
			t.Fatalf("agent_notes = %v, want 'first note'", task["agent_notes"])
		}
	})

	t.Run("DELETE on /api/agent/tasks/:id returns 405", func(t *testing.T) {
		req := testReq(http.MethodDelete, "/api/agent/tasks/"+createdID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405 on Agent DELETE, got %d", rec.Code)
		}
	})
}

func TestAPIIsolation(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())
	projectPath := filepath.Join(t.TempDir(), "my-app")

	task, err := store.CreateTask(map[string]any{"title": "Iso Task", "project_path": projectPath})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	t.Run("Agent cannot access projects", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/agent/projects", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("Agent cannot access stats", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/agent/stats", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("Board cannot access notes endpoint", func(t *testing.T) {
		req := testReq(http.MethodPost, "/api/board/tasks/"+task.ID+"/notes", strings.NewReader(`{"note":"test"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 on board /notes, got %d", rec.Code)
		}
	})
}

func TestMalformedAndValidation(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())
	projectPath := filepath.Join(t.TempDir(), "my-app")

	cases := []struct {
		name string
		body string
	}{
		{"invalid status", fmt.Sprintf(`{"title":"bad status","status":"invalid","project_path":%q}`, projectPath)},
		{"invalid priority", fmt.Sprintf(`{"title":"bad priority","priority":"super","project_path":%q}`, projectPath)},
		{"invalid line range", fmt.Sprintf(`{"title":"bad lines","line_start":50,"line_end":20,"project_path":%q}`, projectPath)},
		{"malformed JSON", `{"title": not valid json`},
		{"JSON array instead of object", `[{"title":"array"}]`},
		{"JSON string instead of object", `"just a string"`},
		{"JSON null instead of object", `null`},
		{"trailing garbage after JSON", `{"title":"valid"} garbage`},
	}

	for _, tc := range cases {
		for _, prefix := range []string{"/api/agent/tasks", "/api/board/tasks"} {
			t.Run(tc.name+" "+prefix, func(t *testing.T) {
				req := testReq(http.MethodPost, prefix, strings.NewReader(tc.body))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("%s (%s): expected 400, got %d: %s", tc.name, prefix, rec.Code, rec.Body.String())
				}
				var res map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
					t.Fatalf("failed to parse json: %v", err)
				}
				if res["success"] != false || res["error"] == nil {
					t.Fatalf("expected error object, got %v", res)
				}
			})
		}
	}

	t.Run("body exceeding 5MB rejected", func(t *testing.T) {
		huge := strings.Repeat("A", 5*1024*1024+10)
		body := fmt.Sprintf(`{"title":"huge","description":"%s"}`, huge)
		req := testReq(http.MethodPost, "/api/agent/tasks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on body exceeding 5MB, got %d", rec.Code)
		}
	})
}

func TestFiltersAndSearch(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	_, err := store.CreateTask(map[string]any{"title": "Search Alpha", "status": "todo", "priority": "high"})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	_, err = store.CreateTask(map[string]any{"title": "Search Beta", "status": "in_progress", "priority": "medium"})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	_, err = store.CreateTask(map[string]any{"title": "Search Gamma", "status": "done", "priority": "low"})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	for _, prefix := range []string{"/api/agent/tasks", "/api/board/tasks"} {
		t.Run("filter by all=true "+prefix, func(t *testing.T) {
			req := testReq(http.MethodGet, prefix+"?all=true", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			var tasks []map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &tasks)
			if len(tasks) != 3 {
				t.Fatalf("expected 3 tasks, got %d", len(tasks))
			}
		})

		t.Run("filter by status "+prefix, func(t *testing.T) {
			req := testReq(http.MethodGet, prefix+"?status=in_progress&all=true", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			var tasks []map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &tasks)
			if len(tasks) != 1 || tasks[0]["title"] != "Search Beta" {
				t.Fatalf("unexpected filtered tasks: %v", tasks)
			}
		})

		t.Run("filter by search query "+prefix, func(t *testing.T) {
			req := testReq(http.MethodGet, prefix+"?q=Gamma&all=true", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			var tasks []map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &tasks)
			if len(tasks) != 1 || tasks[0]["title"] != "Search Gamma" {
				t.Fatalf("unexpected search results: %v", tasks)
			}
		})
	}
}

func TestStartServerPortFallback(t *testing.T) {
	store := newTestServerStore(t)

	// Start first server on port 0 to get an ephemeral free port
	first, err := StartServer(ServerOptions{
		Port:        0,
		OpenBrowser: false,
		DB:          store,
		FS:          testStaticFS(),
	})
	if err != nil {
		t.Fatalf("failed to start first server: %v", err)
	}
	defer first.Close()

	busyPort := first.Port

	// Start second server on busyPort; must fall back to a port in [busyPort+1, busyPort+10]
	second, err := StartServer(ServerOptions{
		Port:        busyPort,
		OpenBrowser: false,
		DB:          store,
		FS:          testStaticFS(),
		MaxAttempts: 10,
	})
	if err != nil {
		t.Fatalf("failed to start second server with fallback: %v", err)
	}
	defer second.Close()

	if second.Port <= busyPort || second.Port > busyPort+10 {
		t.Fatalf("expected second server port to be in [%d, %d], got %d", busyPort+1, busyPort+10, second.Port)
	}

	// Verify both servers respond and ensure response bodies are properly closed
	res1, err := http.Get(first.URL + "/api/health")
	if err != nil || res1.StatusCode != http.StatusOK {
		t.Fatalf("failed to reach first server: %v, status: %v", err, res1)
	}
	_ = res1.Body.Close()

	res2, err := http.Get(second.URL + "/api/health")
	if err != nil || res2.StatusCode != http.StatusOK {
		t.Fatalf("failed to reach second server: %v, status: %v", err, res2)
	}
	_ = res2.Body.Close()
}

func TestStartServerValidation(t *testing.T) {
	store := newTestServerStore(t)

	t.Run("external bind host rejected", func(t *testing.T) {
		_, err := StartServer(ServerOptions{
			Host: "0.0.0.0",
			DB:   store,
		})
		if err == nil || !strings.Contains(err.Error(), "external bind refused") {
			t.Fatalf("expected external bind refused for 0.0.0.0, got %v", err)
		}
	})

	t.Run("nil DB rejected", func(t *testing.T) {
		_, err := StartServer(ServerOptions{
			DB: nil,
		})
		if err == nil || !strings.Contains(err.Error(), "store must not be nil") {
			t.Fatalf("expected store must not be nil, got %v", err)
		}
	})

	t.Run("invalid ports rejected", func(t *testing.T) {
		for _, p := range []int{-1, 65536, 100000} {
			_, err := StartServer(ServerOptions{
				Port: p,
				DB:   store,
			})
			if err == nil || !strings.Contains(err.Error(), "invalid port") {
				t.Fatalf("expected invalid port error for %d, got %v", p, err)
			}
		}
	})
}

// TestHealthReportsAnUnreadableDatabase makes sure the health check does not
// lie: a service that cannot read its database is not healthy.
func TestHealthReportsAnUnreadableDatabase(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := testReq(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}
	if res["success"] != false || res["error"] == nil {
		t.Fatalf("expected a failed health report, got %v", res)
	}
}

// TestCreateTaskRequiresAnAbsoluteProjectPath covers the creation contract:
// the project is named explicitly, and the display name is derived from it.
func TestCreateTaskRequiresAnAbsoluteProjectPath(t *testing.T) {
	for _, prefix := range []string{"/api/agent/tasks", "/api/board/tasks"} {
		prefix := prefix
		t.Run(prefix, func(t *testing.T) {
			store := newTestServerStore(t)
			handler := CreateApp(store, testStaticFS())
			projectPath := filepath.Join(t.TempDir(), "my-app")

			t.Run("missing project path rejected", func(t *testing.T) {
				req := testReq(http.MethodPost, prefix, strings.NewReader(`{"title":"no project"}`))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
				}
			})

			t.Run("relative project path rejected", func(t *testing.T) {
				req := testReq(http.MethodPost, prefix, strings.NewReader(`{"title":"relative","project_path":"some/relative/path"}`))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
				}
			})

			t.Run("camelCase project path accepted", func(t *testing.T) {
				body := fmt.Sprintf(`{"title":"camel","projectPath":%q}`, projectPath)
				req := testReq(http.MethodPost, prefix, strings.NewReader(body))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusCreated {
					t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
				}
				var task map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
					t.Fatalf("failed to parse response: %v", err)
				}
				if task["project_path"] != projectPath {
					t.Fatalf("project_path = %v, want %q", task["project_path"], projectPath)
				}
			})

			t.Run("project name derived from the path", func(t *testing.T) {
				body := fmt.Sprintf(`{"title":"derived","project_path":%q,"project_name":"Custom Name"}`, projectPath)
				req := testReq(http.MethodPost, prefix, strings.NewReader(body))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusCreated {
					t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
				}
				var task map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
					t.Fatalf("failed to parse response: %v", err)
				}
				if task["project_name"] != "my-app" {
					t.Fatalf("project_name = %v, want my-app - the name comes from the path", task["project_name"])
				}
			})
		})
	}
}

// TestTaskListingRequiresAnExplicitScope covers the listing contract: without
// all=true the project has to be named, and it has to be absolute.
func TestTaskListingRequiresAnExplicitScope(t *testing.T) {
	for _, prefix := range []string{"/api/agent/tasks", "/api/board/tasks"} {
		prefix := prefix
		t.Run(prefix, func(t *testing.T) {
			store := newTestServerStore(t)
			handler := CreateApp(store, testStaticFS())
			projectPath := filepath.Join(t.TempDir(), "my-app")

			t.Run("no scope rejected", func(t *testing.T) {
				req := testReq(http.MethodGet, prefix, nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
				}
			})

			t.Run("relative scope rejected", func(t *testing.T) {
				req := testReq(http.MethodGet, prefix+"?project_path=relative/path", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
				}
			})

			t.Run("absolute scope accepted", func(t *testing.T) {
				req := testReq(http.MethodGet, prefix+"?project_path="+url.QueryEscape(projectPath), nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
				}
				var tasks []map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
					t.Fatalf("failed to parse tasks: %v", err)
				}
				if len(tasks) != 0 {
					t.Fatalf("expected no tasks, got %d", len(tasks))
				}
			})

			t.Run("all=true lists every project", func(t *testing.T) {
				req := testReq(http.MethodGet, prefix+"?all=true", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
				}
			})
		})
	}
}

// TestTaskListingPaging checks the bounded page size and the offset.
func TestTaskListingPaging(t *testing.T) {
	for _, prefix := range []string{"/api/agent/tasks", "/api/board/tasks"} {
		prefix := prefix
		t.Run(prefix, func(t *testing.T) {
			store := newTestServerStore(t)
			handler := CreateApp(store, testStaticFS())
			projectPath := filepath.Join(t.TempDir(), "my-app")

			for i := 0; i < 5; i++ {
				if _, err := store.CreateTask(map[string]any{
					"title":        fmt.Sprintf("task %d", i),
					"project_path": projectPath,
				}); err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
			}

			page := func(query string) []map[string]any {
				t.Helper()
				req := testReq(http.MethodGet, prefix+"?project_path="+url.QueryEscape(projectPath)+query, nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
				}
				var tasks []map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
					t.Fatalf("failed to parse tasks: %v", err)
				}
				return tasks
			}

			if got := page("&limit=2"); len(got) != 2 {
				t.Errorf("limit=2 returned %d tasks, want 2", len(got))
			}
			if got := page("&limit=2&offset=2"); len(got) != 2 {
				t.Errorf("limit=2&offset=2 returned %d tasks, want 2", len(got))
			}
			if got := page("&limit=2&offset=4"); len(got) != 1 {
				t.Errorf("limit=2&offset=4 returned %d tasks, want 1", len(got))
			}
			if got := page("&limit=2&offset=99"); len(got) != 0 {
				t.Errorf("offset past the end returned %d tasks, want 0", len(got))
			}

			for _, bad := range []string{"&limit=0", "&limit=501", "&limit=abc", "&offset=-1", "&offset=abc"} {
				req := testReq(http.MethodGet, prefix+"?project_path="+url.QueryEscape(projectPath)+bad, nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s: expected 400, got %d", bad, rec.Code)
				}
			}
		})
	}
}

// TestAppendNotesEndpoint covers the note endpoint: a non-empty note is
// appended to the task, and everything else is refused.
func TestAppendNotesEndpoint(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())
	projectPath := filepath.Join(t.TempDir(), "my-app")

	task, err := store.CreateTask(map[string]any{"title": "Notes", "project_path": projectPath})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	appendNote := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := testReq(http.MethodPost, "/api/agent/tasks/"+id+"/notes", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := appendNote(task.ID, `{"note":"first"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("failed to parse task: %v", err)
	}
	if updated["agent_notes"] != "first" {
		t.Fatalf("agent_notes = %v, want first", updated["agent_notes"])
	}

	rec = appendNote(task.ID, `{"note":"second"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("failed to parse task: %v", err)
	}
	if updated["agent_notes"] != "first\nsecond" {
		t.Fatalf("agent_notes = %v, want first\\nsecond", updated["agent_notes"])
	}

	for _, body := range []string{`{"note":""}`, `{"note":"   "}`, `{}`, `{"note":42}`, `{"note":null}`} {
		rec := appendNote(task.ID, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: expected 400, got %d", body, rec.Code)
		}
	}

	rec = appendNote("task_missing", `{"note":"hello"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown task, got %d", rec.Code)
	}

	req := testReq(http.MethodGet, "/api/agent/tasks/"+task.ID+"/notes", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}
