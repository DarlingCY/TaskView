package board

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

	"taskview/internal/db"
)

// testVersion is what the health endpoint must report in these tests.
const testVersion = "test-version"

func newTestServerStore(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.OpenStore(filepath.Join(dir, "server_test.db"))
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
	handler := CreateApp(store, testStaticFS(), testVersion)

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
	handler := CreateApp(store, testStaticFS(), testVersion)

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
	handler := CreateApp(store, testStaticFS(), testVersion)

	t.Run("GET /api and /api/health", func(t *testing.T) {
		for _, endpoint := range []string{"/api", "/api/health"} {
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
			if res["success"] != true || res["db"] != nil {
				t.Fatalf("unexpected response (db should not be returned): %v", res)
			}
			if res["version"] != testVersion {
				t.Fatalf("version = %v, want %q", res["version"], testVersion)
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
		for _, key := range []string{"total", "backlog", "todo", "in_progress", "review", "done", "open"} {
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
	handler := CreateApp(store, testStaticFS(), testVersion)

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
		// Verify camelCase alias is removed
		if task["projectPath"] != nil || task["createdAt"] != nil {
			t.Fatalf("camelCase aliases should not be present in json: %v", task)
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

func TestAgentAPIRemoved(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
	projectPath := filepath.Join(t.TempDir(), "my-app")

	task, err := store.CreateTask(map[string]any{"title": "Iso Task", "project_path": projectPath})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/agent", ""},
		{http.MethodGet, "/api/agent/health", ""},
		{http.MethodPost, "/api/agent/health", ""},
		{http.MethodGet, "/api/agent/tasks", ""},
		{http.MethodGet, "/api/agent/tasks?project_path=" + url.QueryEscape(projectPath), ""},
		{http.MethodPost, "/api/agent/tasks", `{"title":"test","project_path":""}`},
		{http.MethodGet, "/api/agent/tasks/" + task.ID, ""},
		{http.MethodPatch, "/api/agent/tasks/" + task.ID, `{"status":"in_progress"}`},
		{http.MethodPut, "/api/agent/tasks/" + task.ID, `{"status":"in_progress"}`},
		{http.MethodDelete, "/api/agent/tasks/" + task.ID, ""},
		{http.MethodPost, "/api/agent/tasks/" + task.ID + "/notes", `{"note":"test"}`},
		{http.MethodPost, "/api/agent/tasks/" + task.ID + "/reject", `{"reason":"test"}`},
		{http.MethodHead, "/api/agent/tasks", ""},
		{http.MethodGet, "/api/agent/projects", ""},
		{http.MethodGet, "/api/agent/stats", ""},
		{http.MethodGet, "/api/agent/unknown", ""},
	}

	for _, tc := range endpoints {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			}
			req := testReq(tc.method, tc.path, bodyReader)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for removed agent endpoint %s %s, got %d: %s",
					tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMalformedAndValidation(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
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
		for _, prefix := range []string{"/api/board/tasks"} {
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
		req := testReq(http.MethodPost, "/api/board/tasks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on body exceeding 5MB, got %d", rec.Code)
		}
	})
}

func TestFiltersAndSearch(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
	projectPath := filepath.Join(t.TempDir(), "my-app")

	_, err := store.CreateTask(map[string]any{"title": "Search Alpha", "status": "todo", "priority": "high", "project_path": projectPath})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	_, err = store.CreateTask(map[string]any{"title": "Search Beta", "status": "in_progress", "priority": "medium", "project_path": projectPath})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	_, err = store.CreateTask(map[string]any{"title": "Search Gamma", "status": "done", "priority": "low", "project_path": projectPath})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	t.Run("board filter by all=true", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?all=true", nil)
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

	t.Run("board filter by status", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?status=in_progress&all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var tasks []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &tasks)
		if len(tasks) != 1 || tasks[0]["title"] != "Search Beta" {
			t.Fatalf("unexpected filtered tasks: %v", tasks)
		}
	})

	t.Run("board filter by search query", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?q=Gamma&all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var tasks []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &tasks)
		if len(tasks) != 1 || tasks[0]["title"] != "Search Gamma" {
			t.Fatalf("unexpected search results: %v", tasks)
		}
	})

	t.Run("board filter by status with project_path", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?project_path="+url.QueryEscape(projectPath)+"&status=in_progress", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var tasks []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &tasks)
		if len(tasks) != 1 || tasks[0]["title"] != "Search Beta" {
			t.Fatalf("unexpected filtered tasks: %v", tasks)
		}
	})

	for _, prefix := range []string{"/api/board/tasks"} {
		t.Run("single status and priority "+prefix, func(t *testing.T) {
			req := testReq(http.MethodGet, prefix+"?project_path="+url.QueryEscape(projectPath)+"&status=todo&priority=high", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var tasks []map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
				t.Fatalf("failed to parse tasks: %v", err)
			}
			if len(tasks) != 1 || tasks[0]["title"] != "Search Alpha" {
				t.Fatalf("unexpected filtered tasks: %v", tasks)
			}
		})

		for _, query := range []string{
			"&status=todo,in_progress",
			"&priority=low,high",
			"&status=todo&status=in_progress",
			"&priority=low&priority=high",
			"&status=todo&status=todo",
			"&priority=high&priority=high",
		} {
			t.Run("reject multiple values "+prefix+query, func(t *testing.T) {
				req := testReq(http.MethodGet, prefix+"?project_path="+url.QueryEscape(projectPath)+query, nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// The review stage must work through both HTTP surfaces: an agent can create
// and park a task in review, the board can filter on it, and both can move it
// on to done.
func TestReviewStatusOverHTTP(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
	projectPath := filepath.Join(t.TempDir(), "my-app")

	var createdID string

	t.Run("POST /api/board/tasks creates a review task", func(t *testing.T) {
		body := fmt.Sprintf(`{"title":"Review Task","status":"review","project_path":%q}`, projectPath)
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
		if task["status"] != "review" {
			t.Fatalf("status = %v, want review", task["status"])
		}
		if task["closed_at"] != nil {
			t.Fatalf("closed_at = %v, want null for a review task", task["closed_at"])
		}
		createdID = task["id"].(string)
	})

	t.Run("board filter by status=review", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?project_path="+url.QueryEscape(projectPath)+"&status=review", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var tasks []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
			t.Fatalf("failed to parse tasks: %v", err)
		}
		if len(tasks) != 1 || tasks[0]["id"] != createdID {
			t.Fatalf("unexpected filtered tasks: %v", tasks)
		}
	})

	t.Run("board filter by status=review", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?status=review&all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var tasks []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
			t.Fatalf("failed to parse tasks: %v", err)
		}
		if len(tasks) != 1 || tasks[0]["id"] != createdID {
			t.Fatalf("unexpected filtered tasks: %v", tasks)
		}
	})

	t.Run("board stats count review tasks as open", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/stats", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var stats map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
			t.Fatalf("failed to parse stats: %v", err)
		}
		if stats["review"] != float64(1) {
			t.Fatalf("review = %v, want 1", stats["review"])
		}
		if stats["open"] != float64(1) {
			t.Fatalf("open = %v, want 1 - review is unfinished", stats["open"])
		}
		if stats["done"] != float64(0) {
			t.Fatalf("done = %v, want 0", stats["done"])
		}
	})

	t.Run("PATCH /api/board/tasks/:id moves review to done", func(t *testing.T) {
		body := `{"status":"done"}`
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
		if task["status"] != "done" {
			t.Fatalf("status = %v, want done", task["status"])
		}
		if task["closed_at"] == nil {
			t.Fatal("closed_at should be set once the task is done")
		}
	})

	t.Run("PATCH /api/board/tasks/:id reopens a done task back to review", func(t *testing.T) {
		body := `{"status":"review"}`
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
		if task["status"] != "review" {
			t.Fatalf("status = %v, want review", task["status"])
		}
		if task["closed_at"] != nil {
			t.Fatalf("closed_at = %v, want null after leaving done", task["closed_at"])
		}
	})
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
	handler := CreateApp(store, testStaticFS(), testVersion)

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
	for _, prefix := range []string{"/api/board/tasks"} {
		prefix := prefix
		t.Run(prefix, func(t *testing.T) {
			store := newTestServerStore(t)
			handler := CreateApp(store, testStaticFS(), testVersion)
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
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
	projectPath := filepath.Join(t.TempDir(), "my-app")

	for _, prefix := range []string{"/api/board/tasks"} {
		prefix := prefix
		t.Run("no scope rejected "+prefix, func(t *testing.T) {
			req := testReq(http.MethodGet, prefix, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})

		t.Run("relative scope rejected "+prefix, func(t *testing.T) {
			req := testReq(http.MethodGet, prefix+"?project_path=relative/path", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})

		t.Run("absolute scope accepted "+prefix, func(t *testing.T) {
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
	}

	t.Run("board all=true lists every project", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks?all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

}

// TestTaskListingPaging checks the bounded page size and the offset for board queries.
func TestTaskListingPaging(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
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
		req := testReq(http.MethodGet, "/api/board/tasks?project_path="+url.QueryEscape(projectPath)+query, nil)
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
		req := testReq(http.MethodGet, "/api/board/tasks?project_path="+url.QueryEscape(projectPath)+bad, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", bad, rec.Code)
		}
	}
}

// TestTaskListOmitsBodyFields pins the list/single field difference down at the
// HTTP layer: GET /api/board/tasks must not ship the code snippet or the agent
// notes (the two large text bodies), while GET /api/board/tasks/{id} keeps the
// full task. Every other field, rejection_reason included, stays on both.
func TestTaskListOmitsBodyFields(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
	projectPath := filepath.Join(t.TempDir(), "my-app")

	snippet := "func main() {\n\tprintln(\"hi\")\n}"
	notes := "根本原因：N+1 查询；建议：批量加载。"
	task, err := store.CreateTask(map[string]any{
		"title":        "Body fields task",
		"description":  "a description",
		"status":       "in_progress",
		"priority":     "high",
		"file_path":    "src/main.go",
		"line_start":   3,
		"line_end":     9,
		"code_snippet": snippet,
		"agent_notes":  notes,
		"project_path": projectPath,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := store.RejectTask(task.ID, "needs a test"); err != nil {
		t.Fatalf("RejectTask: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	list := func(t *testing.T) []map[string]any {
		t.Helper()
		req := testReq(http.MethodGet, "/api/board/tasks?project_path="+url.QueryEscape(projectPath), nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var tasks []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
			t.Fatalf("list: parse: %v", err)
		}
		if len(tasks) == 0 {
			t.Fatalf("list: expected at least 1 task")
		}
		return tasks
	}

	listRow := func(t *testing.T, rows []map[string]any, id string) map[string]any {
		t.Helper()
		for _, row := range rows {
			if row["id"] == id {
				return row
			}
		}
		t.Fatalf("task %s is missing from the list: %v", id, rows)
		return nil
	}

	t.Run("list omits code_snippet and agent_notes", func(t *testing.T) {
		row := listRow(t, list(t), task.ID)
		if _, ok := row["code_snippet"]; ok {
			t.Errorf("list response carries code_snippet: %v", row["code_snippet"])
		}
		if _, ok := row["agent_notes"]; ok {
			t.Errorf("list response carries agent_notes: %v", row["agent_notes"])
		}
	})

	t.Run("list keeps has_agent_notes as a boolean", func(t *testing.T) {
		row := listRow(t, list(t), task.ID)
		got, ok := row["has_agent_notes"]
		if !ok {
			t.Fatalf("list response lost has_agent_notes: %v", row)
		}
		if got != true {
			t.Errorf("has_agent_notes = %v, want true", got)
		}
	})

	t.Run("list keeps every other field", func(t *testing.T) {
		row := listRow(t, list(t), task.ID)
		for _, key := range []string{
			"id", "project_path", "project_name", "title", "status", "priority",
			"description", "file_path", "line_start", "line_end", "rejection_reason",
			"created_at", "updated_at", "closed_at",
		} {
			if _, ok := row[key]; !ok {
				t.Errorf("list response lost %q: %v", key, row)
			}
		}
		if row["id"] != task.ID || row["title"] != "Body fields task" || row["rejection_reason"] != "needs a test" {
			t.Errorf("list response fields mismatch: %v", row)
		}
		if row["file_path"] != "src/main.go" || row["line_start"] != float64(3) || row["line_end"] != float64(9) {
			t.Errorf("list response lost the code context: %v", row)
		}
	})

	t.Run("has_agent_notes is false without notes", func(t *testing.T) {
		other, err := store.CreateTask(map[string]any{
			"title":        "No body fields",
			"project_path": projectPath,
		})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		req := testReq(http.MethodGet, "/api/board/tasks/"+other.ID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("single: expected 200, got %d", rec.Code)
		}

		rows := list(t)
		var found map[string]any
		for _, row := range rows {
			if row["id"] == other.ID {
				found = row
			}
		}
		if found == nil {
			t.Fatalf("the second task is missing from the list: %v", rows)
		}
		if found["has_agent_notes"] != false {
			t.Errorf("has_agent_notes = %v, want false", found["has_agent_notes"])
		}
	})

	t.Run("single task keeps the full body", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/board/tasks/"+task.ID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("single: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var row map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
			t.Fatalf("single: parse: %v", err)
		}
		if row["code_snippet"] != snippet {
			t.Errorf("single response code_snippet = %v, want the stored snippet", row["code_snippet"])
		}
		if row["agent_notes"] != notes {
			t.Errorf("single response agent_notes = %v, want the stored notes", row["agent_notes"])
		}
	})
}

// TestRejectEndpoint covers POST /api/board/tasks/{id}/reject
func TestRejectEndpoint(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS(), testVersion)
	projectPath := filepath.Join(t.TempDir(), "my-app")

	for _, prefix := range []string{"/api/board/tasks"} {
		prefix := prefix
		t.Run(prefix+" reject review to in_progress", func(t *testing.T) {
			task, err := store.CreateTask(map[string]any{"title": "Review task", "status": "review", "project_path": projectPath})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			req := testReq(http.MethodPost, prefix+"/"+task.ID+"/reject", strings.NewReader(`{"reason":"Bug in logic"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("failed to parse json: %v", err)
			}
			if res["status"] != "in_progress" {
				t.Errorf("status = %v, want in_progress", res["status"])
			}
			if res["rejection_reason"] != "Bug in logic" {
				t.Errorf("rejection_reason = %v, want 'Bug in logic'", res["rejection_reason"])
			}
		})

		t.Run(prefix+" reject in_progress to backlog", func(t *testing.T) {
			task, err := store.CreateTask(map[string]any{"title": "InProgress task", "status": "in_progress", "project_path": projectPath})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			req := testReq(http.MethodPost, prefix+"/"+task.ID+"/reject", strings.NewReader(`{"reason":"Missing prereq"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("failed to parse json: %v", err)
			}
			if res["status"] != "backlog" {
				t.Errorf("status = %v, want backlog", res["status"])
			}
			if res["rejection_reason"] != "Missing prereq" {
				t.Errorf("rejection_reason = %v, want 'Missing prereq'", res["rejection_reason"])
			}
		})

		t.Run(prefix+" reject todo to backlog", func(t *testing.T) {
			task, err := store.CreateTask(map[string]any{"title": "Todo task", "status": "todo", "project_path": projectPath})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			req := testReq(http.MethodPost, prefix+"/"+task.ID+"/reject", strings.NewReader(`{"reason":"Needs clarification"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("failed to parse json: %v", err)
			}
			if res["status"] != "backlog" {
				t.Errorf("status = %v, want backlog", res["status"])
			}
			if res["rejection_reason"] != "Needs clarification" {
				t.Errorf("rejection_reason = %v, want 'Needs clarification'", res["rejection_reason"])
			}
		})

		t.Run(prefix+" reject validation errors", func(t *testing.T) {
			task, err := store.CreateTask(map[string]any{"title": "Backlog task", "status": "backlog", "project_path": projectPath})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			// Rejecting backlog returns 400
			req := testReq(http.MethodPost, prefix+"/"+task.ID+"/reject", strings.NewReader(`{"reason":"Invalid"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("rejecting backlog: expected 400, got %d", rec.Code)
			}

			// Empty reason returns 400
			req = testReq(http.MethodPost, prefix+"/"+task.ID+"/reject", strings.NewReader(`{"reason":"   "}`))
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("empty reason: expected 400, got %d", rec.Code)
			}

			// Missing reason field returns 400
			req = testReq(http.MethodPost, prefix+"/"+task.ID+"/reject", strings.NewReader(`{}`))
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("missing reason: expected 400, got %d", rec.Code)
			}

			// 404 for unknown task
			req = testReq(http.MethodPost, prefix+"/task_missing/reject", strings.NewReader(`{"reason":"Valid"}`))
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("unknown task: expected 404, got %d", rec.Code)
			}

			// 405 for GET
			req = testReq(http.MethodGet, prefix+"/"+task.ID+"/reject", nil)
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("GET reject: expected 405, got %d", rec.Code)
			}
		})

		t.Run(prefix+" PATCH cannot tamper rejection_reason directly", func(t *testing.T) {
			task, err := store.CreateTask(map[string]any{"title": "Patch task", "status": "todo", "project_path": projectPath})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			req := testReq(http.MethodPatch, prefix+"/"+task.ID, strings.NewReader(`{"rejection_reason":"Tampered"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			// Because rejection_reason is not in writable columns, passing ONLY rejection_reason returns 400 ("No valid fields provided to update")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 when attempting to patch rejection_reason directly, got %d", rec.Code)
			}

			// Passing rejection_reason alongside another field should ignore rejection_reason
			req = testReq(http.MethodPatch, prefix+"/"+task.ID, strings.NewReader(`{"title":"New Title","rejection_reason":"Tampered"}`))
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("failed to parse json: %v", err)
			}
			if res["rejection_reason"] != nil {
				t.Fatalf("rejection_reason should remain null after patch, got %v", res["rejection_reason"])
			}
		})
	}
}
