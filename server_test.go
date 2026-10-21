package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:3333/api/tasks", nil)
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

	t.Run("GET /api and /api/health", func(t *testing.T) {
		for _, endpoint := range []string{"/api", "/api/health"} {
			req := testReq(http.MethodGet, endpoint, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("failed to parse json: %v", err)
			}
			if res["success"] != true || res["db"] != store.FilePath {
				t.Fatalf("unexpected response: %v", res)
			}
		}
	})

	t.Run("GET /api/projects returns [] on empty store", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/projects", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("expected exact '[]' for empty projects, got: %q", rec.Body.String())
		}
	})

	t.Run("GET /api/stats", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/stats", nil)
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

func TestTasksCRUD(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	var createdID string

	t.Run("GET /api/tasks returns [] on empty store", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/tasks", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("expected exact '[]' for empty tasks, got: %q", rec.Body.String())
		}
	})

	t.Run("POST /api/tasks creates task", func(t *testing.T) {
		body := `{"title":"HTTP Test Task","priority":"high","status":"todo"}`
		req := testReq(http.MethodPost, "/api/tasks", strings.NewReader(body))
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

		id, ok := task["id"].(string)
		if !ok || id == "" {
			t.Fatalf("missing or invalid id: %v", task["id"])
		}
		createdID = id
	})

	t.Run("PUT /api/tasks creates task (compatibility)", func(t *testing.T) {
		body := `{"title":"PUT Created Task"}`
		req := testReq(http.MethodPut, "/api/tasks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GET /api/tasks/:id retrieves task", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/tasks/"+createdID, nil)
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

	t.Run("GET /api/tasks/unknown returns 404", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/tasks/task_does_not_exist", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("PATCH /api/tasks/:id updates task", func(t *testing.T) {
		body := `{"status":"in_progress","agentNotes":"working on api test"}`
		req := testReq(http.MethodPatch, "/api/tasks/"+createdID, strings.NewReader(body))
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

	t.Run("PATCH /api/tasks/:id SPA full edit payload clearing nullable fields", func(t *testing.T) {
		body := `{
			"title": "SPA Cleared Task",
			"description": null,
			"filePath": null,
			"lineStart": null,
			"lineEnd": null,
			"codeSnippet": null,
			"agentNotes": null,
			"projectPath": "C:\\Workspace\\TaskView",
			"projectName": "TaskView"
		}`
		req := testReq(http.MethodPatch, "/api/tasks/"+createdID, strings.NewReader(body))
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

	t.Run("PATCH /api/tasks/:id with no valid fields returns 400", func(t *testing.T) {
		body := `{"unrecognizedField":"abc"}`
		req := testReq(http.MethodPatch, "/api/tasks/"+createdID, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("DELETE /api/tasks/:id deletes task", func(t *testing.T) {
		req := testReq(http.MethodDelete, "/api/tasks/"+createdID, nil)
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
		reqGet := testReq(http.MethodGet, "/api/tasks/"+createdID, nil)
		recGet := httptest.NewRecorder()
		handler.ServeHTTP(recGet, reqGet)
		if recGet.Code != http.StatusNotFound {
			t.Fatalf("expected 404 after delete, got %d", recGet.Code)
		}
	})

	t.Run("DELETE nonexistent task returns 404", func(t *testing.T) {
		req := testReq(http.MethodDelete, "/api/tasks/task_already_deleted", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("unsupported method on /api/tasks/:id returns 405", func(t *testing.T) {
		req := testReq(http.MethodPost, "/api/tasks/task_123", nil)
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

func TestMalformedAndValidation(t *testing.T) {
	store := newTestServerStore(t)
	handler := CreateApp(store, testStaticFS())

	cases := []struct {
		name string
		body string
	}{
		{"invalid status", `{"title":"bad status","status":"invalid"}`},
		{"invalid priority", `{"title":"bad priority","priority":"super"}`},
		{"invalid line range", `{"title":"bad lines","line_start":50,"line_end":20}`},
		{"malformed JSON", `{"title": not valid json`},
		{"JSON array instead of object", `[{"title":"array"}]`},
		{"JSON string instead of object", `"just a string"`},
		{"JSON null instead of object", `null`},
		{"trailing garbage after JSON", `{"title":"valid"} garbage`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testReq(http.MethodPost, "/api/tasks", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: expected 400, got %d: %s", tc.name, rec.Code, rec.Body.String())
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

	t.Run("body exceeding 5MB rejected", func(t *testing.T) {
		huge := strings.Repeat("A", 5*1024*1024+10)
		body := fmt.Sprintf(`{"title":"huge","description":"%s"}`, huge)
		req := testReq(http.MethodPost, "/api/tasks", strings.NewReader(body))
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

	t.Run("filter by all=true", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/tasks?all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var tasks []map[string]any
		json.Unmarshal(rec.Body.Bytes(), &tasks)
		if len(tasks) != 3 {
			t.Fatalf("expected 3 tasks, got %d", len(tasks))
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/tasks?status=in_progress&all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var tasks []map[string]any
		json.Unmarshal(rec.Body.Bytes(), &tasks)
		if len(tasks) != 1 || tasks[0]["title"] != "Search Beta" {
			t.Fatalf("unexpected filtered tasks: %v", tasks)
		}
	})

	t.Run("filter by search query", func(t *testing.T) {
		req := testReq(http.MethodGet, "/api/tasks?q=Gamma&all=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var tasks []map[string]any
		json.Unmarshal(rec.Body.Bytes(), &tasks)
		if len(tasks) != 1 || tasks[0]["title"] != "Search Gamma" {
			t.Fatalf("unexpected search results: %v", tasks)
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
