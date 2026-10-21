package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// endpointInfo is public discovery metadata; control credentials stay in server.json.
type endpointInfo struct {
	URL      string `json:"url"`
	DB       string `json:"db"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

func writeEndpoint(dir string, state *ServerState) error {
	return writeJSONAtomic(filepath.Join(dir, "endpoint.json"), endpointInfo{state.URL, state.DB, state.Version, state.Instance})
}

func removeEndpoint(dir, instance string) {
	name := filepath.Join(dir, "endpoint.json")
	var endpoint endpointInfo
	raw, err := os.ReadFile(name)
	if err == nil && json.Unmarshal(raw, &endpoint) == nil && endpoint.Instance == instance {
		_ = os.Remove(name)
	}
}

func writeJSONAtomic(name string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".taskview-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), name)
}

type applicationOptions struct {
	mode       string
	dataDir    string
	port       int
	background bool
	values     map[string]string
}

func parseApplicationOptions(args []string) (applicationOptions, error) {
	seen := make(map[string]bool)
	o := applicationOptions{port: 3333, values: map[string]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if seen[arg] {
			return o, fmt.Errorf("duplicate argument %s", arg)
		}
		seen[arg] = true
		switch arg {
		case "--background":
			o.background = true
		case "--internal-run", "--internal-install-prepare", "--internal-install-worker", "--internal-install-commit", "--internal-install-abort", "--internal-uninstall":
			if o.mode != "" {
				return o, errors.New("conflicting internal modes")
			}
			o.mode = arg
		case "--allow-legacy":
			o.values[arg] = "true"
		case "--data-dir", "--port", "--target", "--transaction", "--autostart", "--start":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
				return o, fmt.Errorf("missing value for %s", arg)
			}
			i++
			o.values[arg] = args[i]
		default:
			return o, errors.New("TaskView is an installed desktop application; task operations use the local HTTP API")
		}
	}
	if dir := o.values["--data-dir"]; dir != "" {
		if !filepath.IsAbs(dir) {
			return o, errors.New("data directory must be absolute")
		}
		o.dataDir = filepath.Clean(dir)
	}
	if p := o.values["--port"]; p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return o, errors.New("invalid internal port")
		}
		if err = ValidatePort(n); err != nil {
			return o, err
		}
		o.port = n
	}
	allowed := map[string]bool{"--data-dir": true}
	var required []string
	switch o.mode {
	case "--internal-run":
		allowed["--port"] = true
	case "--internal-install-prepare":
		required = []string{"--target", "--transaction"}
		allowed["--allow-legacy"] = true
	case "--internal-install-worker", "--internal-install-abort":
		required = []string{"--transaction"}
	case "--internal-install-commit":
		required = []string{"--transaction", "--autostart", "--start"}
	case "--internal-uninstall":
		required = []string{"--target"}
	}
	for _, key := range required {
		allowed[key] = true
		if o.values[key] == "" {
			return o, fmt.Errorf("missing %s", key)
		}
	}
	for key := range o.values {
		if !allowed[key] {
			return o, fmt.Errorf("argument %s is not valid in this mode", key)
		}
	}
	if o.background && o.mode != "" {
		return o, errors.New("background launch cannot be combined with an internal mode")
	}
	return o, nil
}

func runApplication(args []string) error {
	o, err := parseApplicationOptions(args)
	if err != nil {
		return err
	}
	if o.dataDir != "" {
		if err = os.Setenv("TASKVIEW_HOME", o.dataDir); err != nil {
			return err
		}
	}
	switch o.mode {
	case "--internal-install-prepare", "--internal-install-worker", "--internal-install-commit", "--internal-install-abort", "--internal-uninstall":
		return runInstallMode(o)
	case "--internal-run":
		go cleanupStaleUpdateStaging(time.Now())
		return runManagedServer(o.port, runPlatformTray)
	}
	state, err := EnsureBackgroundServer(o.port)
	if err != nil {
		return err
	}
	if state.Version != appVersion() {
		return errors.New("another TaskView version is running; close it or upgrade using the installer")
	}
	if !o.background && !OpenBrowser(state.URL) {
		return errors.New("TaskView is running, but the browser could not be opened")
	}
	return nil
}

func checkForApplicationUpdate(ctx context.Context) error {
	u := NewUpdater()
	release, err := u.Check(ctx)
	if errors.Is(err, ErrNoRelease) {
		platformNotify("TaskView 更新", "暂无正式发布版本。")
		return nil
	}
	if err != nil {
		return err
	}
	if release == nil {
		platformNotify("TaskView 更新", "当前已是最新版本。")
		return nil
	}
	// A silent installer stops at the migrate step for a legacy installation
	// and would fail without any visible result. Refuse up front instead.
	if pending, err := legacyInstallationPending(); err == nil && pending {
		platformNotify("TaskView 更新", "检测到旧版 TaskView 的残留安装（便携版或旧版开机自启）。\n\n自动更新不会替你迁移旧版：请先卸载旧版本，再手动运行安装程序完成升级。任务数据不会受影响。")
		return nil
	}
	if !platformConfirm("TaskView 更新", fmt.Sprintf("当前版本：%s\n新版本：%s\n\n下载校验后将运行升级安装程序，暂时关闭并重启 TaskView。任务数据和自启设置保留。\n是否继续？", appVersion(), release.Version)) {
		return nil
	}
	installer, cleanup, err := u.Download(ctx, release)
	if err != nil {
		return err
	}
	if err = launchUpdateInstaller(ctx, installer); err != nil {
		cleanup()
		return err
	}
	// The verified installer must remain available after this process exits:
	// the upgrade stops TaskView and this goroutine dies with it. Stale staging
	// directories are therefore collected on the next start instead of here.
	return nil
}

// cleanupStaleUpdateStaging removes leftover download directories from earlier
// upgrades.
//
// Only directories this application created are considered - the updater uses
// a dedicated prefix under the system temporary directory - and only ones that
// are old enough that no upgrade can still be using them. Failures are ignored:
// a leftover directory costs disk space, never correctness.
func cleanupStaleUpdateStaging(now time.Time) {
	root := os.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "taskview-update-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < 6*time.Hour {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

func waitForApplicationVersion(ctx context.Context, expected, executable string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if state, err := ManagedServerStatus(); err == nil && state != nil && state.Version == expected && (expected == "" || samePath(state.Executable, executable)) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, state.URL+"/api/health", nil)
			if err != nil {
				return err
			}
			resp, err := newControlHTTPClient(time.Second).Do(req)
			if err == nil {
				var health struct {
					Success bool   `json:"success"`
					Version string `json:"version"`
				}
				decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&health)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && decodeErr == nil && health.Success && (expected == "" || health.Version == expected) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
