package main

// The Board HTTP layer and its tests live in taskview/internal/board, which
// builds and runs on every platform. What cannot move is the embedded UI
// bundle: //go:embed can only reference files in the declaring package's own
// directory, so src/public stays embedded here (assets.go) and is passed into
// the board package. This test pins that wiring down.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedAssetsServeTailwindCSS(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "server_test.db"))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// A nil fs is resolved by the root wrapper to getPublicFS(), the embedded
	// src/public bundle.
	handler := CreateApp(store, nil)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/tailwind.min.css", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for embedded tailwind.min.css, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("expected text/css, got %s", rec.Header().Get("Content-Type"))
	}
	if rec.Body.Len() < 5000 {
		t.Fatalf("embedded tailwind.min.css is unexpectedly small: %d bytes", rec.Body.Len())
	}
}
