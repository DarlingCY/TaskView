package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installed application is started as a desktop program, so the argument
// parser is the only way into the internal modes. A mistake here either
// breaks an upgrade or lets an unknown command through.
func TestParseApplicationOptions(t *testing.T) {
	abs := t.TempDir()
	cases := []struct {
		name    string
		args    []string
		mode    string
		port    int
		dataDir string
		bg      bool
		values  map[string]string
		wantErr string
	}{
		{name: "no arguments", port: 3333},
		{name: "background launch", args: []string{"--background"}, port: 3333, bg: true},
		{
			name:    "internal run",
			args:    []string{"--internal-run", "--port", "4000", "--data-dir", abs},
			mode:    "--internal-run",
			port:    4000,
			dataDir: abs,
			values:  map[string]string{"--port": "4000", "--data-dir": abs},
		},
		{
			name:   "install commit",
			args:   []string{"--internal-install-commit", "--transaction", `C:\tmp\taskview-transaction.json`, "--autostart", "preserve", "--start", "no"},
			mode:   "--internal-install-commit",
			port:   3333,
			values: map[string]string{"--transaction": `C:\tmp\taskview-transaction.json`, "--autostart": "preserve", "--start": "no"},
		},
		{
			name:   "install prepare with legacy confirmation",
			args:   []string{"--internal-install-prepare", "--target", `C:\Program Files\TaskView.exe`, "--transaction", `C:\tmp\taskview-transaction.json`, "--allow-legacy"},
			mode:   "--internal-install-prepare",
			port:   3333,
			values: map[string]string{"--target": `C:\Program Files\TaskView.exe`, "--transaction": `C:\tmp\taskview-transaction.json`, "--allow-legacy": "true"},
		},
		{name: "duplicate argument", args: []string{"--port", "1", "--port", "2"}, wantErr: "duplicate argument --port"},
		{name: "unknown argument", args: []string{"add"}, wantErr: "installed desktop application"},
		{name: "missing value", args: []string{"--data-dir"}, wantErr: "missing value for --data-dir"},
		{name: "value looks like a flag", args: []string{"--internal-install-prepare", "--target", "--transaction", "x"}, wantErr: "missing value for --target"},
		{name: "relative data directory", args: []string{"--data-dir", "relative/path"}, wantErr: "data directory must be absolute"},
		{name: "invalid port", args: []string{"--internal-run", "--port", "abc"}, wantErr: "invalid internal port"},
		{name: "port out of range", args: []string{"--internal-run", "--port", "70000"}, wantErr: "invalid port 70000"},
		{name: "missing required argument", args: []string{"--internal-install-prepare", "--target", `C:\TaskView.exe`}, wantErr: "missing --transaction"},
		{name: "argument not valid in this mode", args: []string{"--internal-run", "--target", `C:\TaskView.exe`}, wantErr: "argument --target is not valid in this mode"},
		{name: "background with an internal mode", args: []string{"--internal-run", "--background"}, wantErr: "background launch cannot be combined with an internal mode"},
		{name: "conflicting internal modes", args: []string{"--internal-run", "--internal-uninstall", "--target", `C:\TaskView.exe`}, wantErr: "conflicting internal modes"},
		{name: "commit without a launch decision", args: []string{"--internal-install-commit", "--transaction", `C:\t`, "--autostart", "preserve"}, wantErr: "missing --start"},
		{name: "legacy confirmation outside the prepare mode", args: []string{"--internal-install-commit", "--transaction", `C:\t`, "--autostart", "preserve", "--start", "no", "--allow-legacy"}, wantErr: "argument --allow-legacy is not valid in this mode"},
		{name: "worker without a transaction", args: []string{"--internal-install-worker"}, wantErr: "missing --transaction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseApplicationOptions(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseApplicationOptions(%v) = %+v, want an error containing %q", tc.args, o, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseApplicationOptions(%v) error = %q, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseApplicationOptions(%v) = %v", tc.args, err)
			}
			if o.mode != tc.mode {
				t.Errorf("mode = %q, want %q", o.mode, tc.mode)
			}
			if o.port != tc.port {
				t.Errorf("port = %d, want %d", o.port, tc.port)
			}
			if o.dataDir != tc.dataDir {
				t.Errorf("data directory = %q, want %q", o.dataDir, tc.dataDir)
			}
			if o.background != tc.bg {
				t.Errorf("background = %v, want %v", o.background, tc.bg)
			}
			if len(o.values) != len(tc.values) {
				t.Errorf("values = %v, want %v", o.values, tc.values)
			}
			for key, want := range tc.values {
				if o.values[key] != want {
					t.Errorf("values[%q] = %q, want %q", key, o.values[key], want)
				}
			}
		})
	}
}

// The endpoint file is public discovery metadata: it tells the browser where
// the application lives. The control token belongs to the private server
// metadata and must never end up in a file the browser can read.
func TestEndpointMetadataIsTokenFree(t *testing.T) {
	dir := t.TempDir()
	state := &ServerState{
		URL:      "http://127.0.0.1:3333",
		DB:       filepath.Join(dir, "tasks.db"),
		Token:    "control-token-secret",
		Instance: "instance-abc",
		Version:  "1.2.3",
	}
	if err := writeEndpoint(dir, state); err != nil {
		t.Fatalf("writeEndpoint: %v", err)
	}
	name := filepath.Join(dir, "endpoint.json")
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, state.Token) {
		t.Fatalf("endpoint.json leaks the control token: %s", text)
	}
	if strings.Contains(text, `"token"`) {
		t.Fatalf("endpoint.json carries a token field: %s", text)
	}
	var info endpointInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decode endpoint: %v", err)
	}
	if info.URL != state.URL || info.DB != state.DB || info.Version != state.Version || info.Instance != state.Instance {
		t.Fatalf("endpoint.json = %+v, want %+v", info, *state)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "endpoint.json" {
		t.Fatalf("data directory holds %v, want only endpoint.json", entries)
	}

	// Another instance must not delete the endpoint of this one.
	removeEndpoint(dir, "instance-other")
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("removeEndpoint deleted another instance's endpoint: %v", err)
	}
	removeEndpoint(dir, state.Instance)
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("removeEndpoint kept the endpoint of its own instance: %v", err)
	}
}
