// Tests for the update check and download.
//
// Every test runs against a local httptest server. The client used by the
// tests rewrites the outgoing URL to that server while keeping the logical
// GitHub URL (host, scheme and path) intact, so the production URL checks run
// exactly as they would against github.com and nothing ever leaves the
// machine.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// rewriteTransport sends every request to the local test server while keeping
// the logical URL untouched. This keeps the host, scheme and path checks of
// the production code meaningful.
type rewriteTransport struct {
	base *url.URL
	next http.RoundTripper

	mu   sync.Mutex
	seen []string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.seen = append(t.seen, req.URL.String())
	t.mu.Unlock()

	clone := req.Clone(req.Context())
	target := *clone.URL
	target.Scheme = t.base.Scheme
	target.Host = t.base.Host
	clone.URL = &target
	clone.Host = ""

	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(clone)
}

// urlsSeen returns the logical URLs the client requested.
func (t *rewriteTransport) urlsSeen() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.seen))
	copy(out, t.seen)
	return out
}

// fakeAsset is a release asset served by the fake GitHub.
type fakeAsset struct {
	name string
	url  string
	size int64
}

// fakeRelease is the release metadata served by the fake GitHub.
type fakeRelease struct {
	tag        string
	htmlURL    string
	body       string
	draft      bool
	prerelease bool
	assets     []fakeAsset
}

// marshal renders the release as the GitHub API would.
func (f fakeRelease) marshal(t *testing.T) string {
	t.Helper()
	assets := make([]map[string]any, 0, len(f.assets))
	for _, asset := range f.assets {
		assets = append(assets, map[string]any{
			"name":                 asset.name,
			"browser_download_url": asset.url,
			"size":                 asset.size,
		})
	}
	payload := map[string]any{
		"tag_name":   f.tag,
		"html_url":   f.htmlURL,
		"body":       f.body,
		"draft":      f.draft,
		"prerelease": f.prerelease,
		"assets":     assets,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fake release: %v", err)
	}
	return string(data)
}

// fakeGitHub is a local stand-in for the GitHub release endpoints.
type fakeGitHub struct {
	server    *httptest.Server
	client    *http.Client
	transport *rewriteTransport
}

// newFakeGitHub starts a local server and returns a client that talks to it.
func newFakeGitHub(t *testing.T, handler http.Handler) *fakeGitHub {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	transport := &rewriteTransport{base: base, next: http.DefaultTransport}
	return &fakeGitHub{
		server:    server,
		client:    &http.Client{Transport: transport},
		transport: transport,
	}
}

const (
	// releasePath is the path of the "latest release" endpoint.
	releasePath = "/repos/DarlingCY/TaskView/releases/latest"
	// testTag and testVersion describe the release used by most tests.
	testTag     = "v1.3.0"
	testVersion = "1.3.0"
)

// testInstallerName is the published name of the installer of testTag.
var testInstallerName = fmt.Sprintf(installerAssetPattern, testVersion)

// githubAssetURL builds the canonical download URL of a release asset.
func githubAssetURL(tag string, name string) string {
	return "https://github.com/DarlingCY/TaskView/releases/download/" + tag + "/" + name
}

// assetPath is the path the fake server serves an asset under.
func assetPath(tag string, name string) string {
	return "/DarlingCY/TaskView/releases/download/" + tag + "/" + name
}

// standardRelease is a healthy release with both required assets.
func standardRelease(tag string) fakeRelease {
	name := fmt.Sprintf(installerAssetPattern, strings.TrimPrefix(tag, "v"))
	return fakeRelease{
		tag:     tag,
		htmlURL: "https://github.com/DarlingCY/TaskView/releases/tag/" + tag,
		body:    "Bug fixes and improvements.",
		assets: []fakeAsset{
			{name: name, url: githubAssetURL(tag, name), size: 5},
			{name: checksumAssetName, url: githubAssetURL(tag, checksumAssetName), size: 96},
		},
	}
}

// newFakeGitHubServer serves a release and its assets.
func newFakeGitHubServer(t *testing.T, release fakeRelease, files map[string][]byte) *fakeGitHub {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, release.marshal(t))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	return newFakeGitHub(t, mux)
}

// checksumLine renders a SHA256SUMS line for a payload.
func checksumLine(payload []byte, name string) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
}

// withLimit temporarily lowers one of the size limits.
func withLimit(t *testing.T, target *int64, value int64) {
	t.Helper()
	original := *target
	*target = value
	t.Cleanup(func() { *target = original })
}

// stagingDirs lists the staging directories currently in the temp directory.
func stagingDirs(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp directory: %v", err)
	}
	dirs := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), stagingPrefix) {
			dirs[entry.Name()] = true
		}
	}
	return dirs
}

// ---------------------------------------------------------------------------
// Defaults and version handling
// ---------------------------------------------------------------------------

func TestNewUpdaterUsesFixedDefaults(t *testing.T) {
	updater := NewUpdater()
	if updater.APIURL != defaultAPIURL {
		t.Fatalf("expected the fixed GitHub endpoint %q, got %q", defaultAPIURL, updater.APIURL)
	}
	if updater.CurrentVersion != appVersion() {
		t.Fatalf("expected the current version %q, got %q", appVersion(), updater.CurrentVersion)
	}
	if updater.Client == nil {
		t.Fatal("expected a default HTTP client")
	}
}

func TestParseStableVersion(t *testing.T) {
	valid := []struct {
		input string
		want  string
	}{
		{"v1.3.0", "1.3.0"},
		{"1.3.0", "1.3.0"},
		{"v0.0.1", "0.0.1"},
		{"v10.20.30", "10.20.30"},
		{" v1.2.3 ", "1.2.3"},
	}
	for _, tc := range valid {
		version, err := parseStableVersion(tc.input)
		if err != nil {
			t.Fatalf("parseStableVersion(%q) failed: %v", tc.input, err)
		}
		if version.String() != tc.want {
			t.Fatalf("parseStableVersion(%q) = %q, want %q", tc.input, version.String(), tc.want)
		}
	}

	invalid := []string{
		"", "dev", "v1.3", "v1.3.0.1", "v1.3.0-beta.1", "v1.3.0+build", "1.3", "latest",
		"v01.3.0", "v1.03.0", "x1.3.0", "v-1.3.0",
	}
	for _, input := range invalid {
		if _, err := parseStableVersion(input); err == nil {
			t.Fatalf("parseStableVersion(%q) should have failed", input)
		}
	}
}

func TestStableVersionCompare(t *testing.T) {
	cases := []struct {
		a    string
		b    string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.1.0", "v1.0.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.0", "v2.0.0", -1},
	}
	for _, tc := range cases {
		a, err := parseStableVersion(tc.a)
		if err != nil {
			t.Fatalf("parseStableVersion(%q): %v", tc.a, err)
		}
		b, err := parseStableVersion(tc.b)
		if err != nil {
			t.Fatalf("parseStableVersion(%q): %v", tc.b, err)
		}
		if got := a.compare(b); got != tc.want {
			t.Fatalf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Check
// ---------------------------------------------------------------------------

func TestCheckReturnsNilWhenUpToDate(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease("v1.2.3"), nil)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if release != nil {
		t.Fatalf("expected no release for an up to date build, got %+v", release)
	}
}

func TestCheckReturnsNewerRelease(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease(testTag), nil)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if release == nil {
		t.Fatal("expected a release")
	}
	if release.Version != testTag {
		t.Fatalf("expected version %q, got %q", testTag, release.Version)
	}
	if release.InstallerName != testInstallerName {
		t.Fatalf("expected installer name %q, got %q", testInstallerName, release.InstallerName)
	}
	if release.InstallerURL != githubAssetURL(testTag, testInstallerName) {
		t.Fatalf("unexpected installer URL %q", release.InstallerURL)
	}
	if release.ChecksumURL != githubAssetURL(testTag, checksumAssetName) {
		t.Fatalf("unexpected checksum URL %q", release.ChecksumURL)
	}
	if release.PageURL != "https://github.com/DarlingCY/TaskView/releases/tag/"+testTag {
		t.Fatalf("unexpected page URL %q", release.PageURL)
	}
	if release.Notes != standardRelease(testTag).body {
		t.Fatalf("unexpected notes %q", release.Notes)
	}

	seen := github.transport.urlsSeen()
	if len(seen) == 0 || seen[0] != defaultAPIURL {
		t.Fatalf("expected the request to go to %q, got %v", defaultAPIURL, seen)
	}
}

func TestCheckReportsNoReleaseOn404(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	github := newFakeGitHub(t, mux)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("expected ErrNoRelease, got %v", err)
	}
	if release != nil {
		t.Fatalf("expected no release alongside ErrNoRelease, got %+v", release)
	}
}

func TestCheckRejectsNonStableVersions(t *testing.T) {
	cases := []struct {
		name    string
		current string
	}{
		{name: "development build", current: "dev"},
		{name: "pre-release", current: "v1.3.0-beta.1"},
		{name: "plain number", current: "1.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHubServer(t, standardRelease(testTag), nil)
			updater := &Updater{Client: github.client, CurrentVersion: tc.current, APIURL: defaultAPIURL}

			release, err := updater.Check(context.Background())
			if err == nil {
				t.Fatalf("expected an error for current version %q", tc.current)
			}
			if release != nil {
				t.Fatalf("expected no release, got %+v", release)
			}
			if !strings.Contains(err.Error(), "stable") {
				t.Fatalf("expected the error to mention a stable version, got: %v", err)
			}
		})
	}
}

// TestCheckFallsBackToTheBuildVersion covers an updater without an explicit
// current version. It follows whatever appVersion reports, so it holds for a
// development build and for a release build alike.
func TestCheckFallsBackToTheBuildVersion(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease(testTag), nil)
	updater := &Updater{Client: github.client, APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if _, parseErr := parseStableVersion(appVersion()); parseErr != nil {
		if err == nil {
			t.Fatal("expected an error for a development build")
		}
		if release != nil {
			t.Fatalf("expected no release, got %+v", release)
		}
		return
	}
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
}

func TestCheckRejectsUnstableReleases(t *testing.T) {
	preReleaseTag := standardRelease("v1.3.0-rc.1")
	draft := standardRelease(testTag)
	draft.draft = true
	preRelease := standardRelease(testTag)
	preRelease.prerelease = true

	for _, tc := range []struct {
		name    string
		release fakeRelease
	}{
		{"pre-release tag", preReleaseTag},
		{"draft", draft},
		{"pre-release flag", preRelease},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHubServer(t, tc.release, nil)
			updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

			release, err := updater.Check(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if release != nil {
				t.Fatalf("expected no release, got %+v", release)
			}
		})
	}
}

func TestCheckRejectsDowngrade(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease("v1.3.0"), nil)
	updater := &Updater{Client: github.client, CurrentVersion: "v2.0.0", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error for a downgrade")
	}
	if release != nil {
		t.Fatalf("expected no release, got %+v", release)
	}
	if !strings.Contains(err.Error(), "older") {
		t.Fatalf("expected the error to explain the downgrade, got: %v", err)
	}
}

func TestCheckRejectsMissingAssets(t *testing.T) {
	release := standardRelease(testTag)
	withoutInstaller := release
	withoutInstaller.assets = []fakeAsset{release.assets[1]}
	withoutChecksums := release
	withoutChecksums.assets = []fakeAsset{release.assets[0]}

	for _, tc := range []struct {
		name    string
		release fakeRelease
	}{
		{"missing installer", withoutInstaller},
		{"missing checksum file", withoutChecksums},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHubServer(t, tc.release, nil)
			updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

			if _, err := updater.Check(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestCheckRejectsUntrustedAssetURLs(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(release *fakeRelease)
		message string
	}{
		{
			name: "foreign host",
			mutate: func(release *fakeRelease) {
				release.assets[0].url = "https://evil.example.com/TaskView-Setup-1.3.0-windows-amd64.exe"
			},
			message: "not a GitHub host",
		},
		{
			name: "plain http",
			mutate: func(release *fakeRelease) {
				release.assets[0].url = "http://github.com/DarlingCY/TaskView/releases/download/v1.3.0/" + testInstallerName
			},
			message: "https",
		},
		{
			name: "another repository",
			mutate: func(release *fakeRelease) {
				release.assets[0].url = "https://github.com/other/repo/releases/download/v1.3.0/" + testInstallerName
			},
			message: "must point at",
		},
		{
			name: "another asset name",
			mutate: func(release *fakeRelease) {
				release.assets[0].url = githubAssetURL(testTag, "other.exe")
			},
			message: "must point at",
		},
		{
			name: "credentials in the url",
			mutate: func(release *fakeRelease) {
				release.assets[0].url = "https://user:secret@github.com/DarlingCY/TaskView/releases/download/v1.3.0/" + testInstallerName
			},
			message: "credentials",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := standardRelease(testTag)
			tc.mutate(&release)
			github := newFakeGitHubServer(t, release, nil)
			updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

			_, err := updater.Check(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("expected the error to mention %q, got: %v", tc.message, err)
			}
		})
	}
}

func TestCheckRejectsForeignAPIEndpoint(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease(testTag), nil)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: "https://evil.example.com/releases/latest"}

	_, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error for a foreign endpoint")
	}
	if !strings.Contains(err.Error(), "not a GitHub host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckRejectsInsecureAPIEndpoint(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease(testTag), nil)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: "http://api.github.com/repos/DarlingCY/TaskView/releases/latest"}

	_, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error for a plain http endpoint")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckAcceptsInjectedLoopbackEndpoint(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease(testTag), nil)
	updater := &Updater{
		Client:         github.client,
		CurrentVersion: "v1.2.3",
		APIURL:         github.server.URL + releasePath,
	}

	release, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if release == nil {
		t.Fatal("expected a release")
	}
}

func TestCheckReportsAPIFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"server boom"}`)
	})
	github := newFakeGitHub(t, mux)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	_, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "server boom") {
		t.Fatalf("expected a readable error, got: %v", err)
	}
}

func TestCheckRejectsOversizedAPIResponse(t *testing.T) {
	github := newFakeGitHubServer(t, standardRelease(testTag), nil)
	withLimit(t, &maxAPIBodyBytes, 16)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	_, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected the error to mention the size limit, got: %v", err)
	}
}

func TestCheckRejectsRedirectToForeignHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/releases/latest", http.StatusFound)
	})
	github := newFakeGitHub(t, mux)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	_, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected the error to mention the redirect, got: %v", err)
	}
}

func TestCheckRejectsTooManyRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://api.github.com/loop", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://api.github.com/loop", http.StatusFound)
	})
	github := newFakeGitHub(t, mux)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	_, err := updater.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("expected the error to mention the redirect limit, got: %v", err)
	}
}

func TestCheckFollowsRedirectToGitHubHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://objects.githubusercontent.com/redirected/latest", http.StatusFound)
	})
	mux.HandleFunc("/redirected/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, standardRelease(testTag).marshal(t))
	})
	github := newFakeGitHub(t, mux)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if release == nil {
		t.Fatal("expected a release")
	}
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// downloadFixture serves a release with a real installer payload.
type downloadFixture struct {
	updater *Updater
	release *UpdateRelease
}

func newDownloadFixture(t *testing.T, sums string, payload []byte) *downloadFixture {
	t.Helper()
	files := map[string][]byte{
		assetPath(testTag, testInstallerName): payload,
		assetPath(testTag, checksumAssetName): []byte(sums),
	}
	github := newFakeGitHubServer(t, standardRelease(testTag), files)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if release == nil {
		t.Fatal("expected a release")
	}
	return &downloadFixture{updater: updater, release: release}
}

func TestDownloadVerifiesAndReturnsInstaller(t *testing.T) {
	payload := []byte("this is the installer")
	fixture := newDownloadFixture(t, checksumLine(payload, testInstallerName), payload)

	before := stagingDirs(t)
	path, cleanup, err := fixture.updater.Download(context.Background(), fixture.release)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected a cleanup function")
	}
	if filepath.Base(path) != testInstallerName {
		t.Fatalf("expected the file %q, got %q", testInstallerName, filepath.Base(path))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded installer: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected the payload %q, got %q", payload, got)
	}

	staging := filepath.Dir(path)
	cleanup()
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("expected the staging directory %q to be removed", staging)
	}
	if after := stagingDirs(t); len(after) != len(before) {
		t.Fatalf("expected no leftover staging directories, before %v after %v", before, after)
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	payload := []byte("this is the installer")
	wrong := checksumLine([]byte("something else"), testInstallerName)
	fixture := newDownloadFixture(t, wrong, payload)

	before := stagingDirs(t)
	path, cleanup, err := fixture.updater.Download(context.Background(), fixture.release)
	if err == nil {
		t.Fatal("expected a checksum error")
	}
	if path != "" {
		t.Fatalf("expected no path, got %q", path)
	}
	if cleanup == nil {
		t.Fatal("expected a cleanup function")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum mismatch error, got: %v", err)
	}
	if after := stagingDirs(t); len(after) != len(before) {
		t.Fatalf("expected no leftover staging directories, before %v after %v", before, after)
	}
}

func TestDownloadRejectsMissingChecksumEntry(t *testing.T) {
	payload := []byte("this is the installer")
	fixture := newDownloadFixture(t, checksumLine(payload, "other-file.exe"), payload)

	_, _, err := fixture.updater.Download(context.Background(), fixture.release)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("expected a missing entry error, got: %v", err)
	}
}

func TestDownloadRejectsDuplicateChecksumEntries(t *testing.T) {
	payload := []byte("this is the installer")
	sums := checksumLine(payload, testInstallerName) + checksumLine([]byte("other"), testInstallerName)
	fixture := newDownloadFixture(t, sums, payload)

	_, _, err := fixture.updater.Download(context.Background(), fixture.release)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "times") {
		t.Fatalf("expected an ambiguous checksum error, got: %v", err)
	}
}

func TestDownloadRejectsOversizedInstaller(t *testing.T) {
	payload := []byte("this is the installer")
	fixture := newDownloadFixture(t, checksumLine(payload, testInstallerName), payload)
	withLimit(t, &maxInstallerBytes, 4)

	before := stagingDirs(t)
	_, _, err := fixture.updater.Download(context.Background(), fixture.release)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected a size limit error, got: %v", err)
	}
	if after := stagingDirs(t); len(after) != len(before) {
		t.Fatalf("expected no leftover staging directories, before %v after %v", before, after)
	}
}

func TestDownloadRejectsOversizedChecksumFile(t *testing.T) {
	payload := []byte("this is the installer")
	fixture := newDownloadFixture(t, checksumLine(payload, testInstallerName), payload)
	withLimit(t, &maxChecksumBytes, 8)

	_, _, err := fixture.updater.Download(context.Background(), fixture.release)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected a size limit error, got: %v", err)
	}
}

func TestDownloadHonoursContextCancellation(t *testing.T) {
	payload := []byte("this is the installer")

	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, standardRelease(testTag).marshal(t))
	})
	mux.HandleFunc(assetPath(testTag, checksumAssetName), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, checksumLine(payload, testInstallerName))
	})
	mux.HandleFunc(assetPath(testTag, testInstallerName), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:4])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	github := newFakeGitHub(t, mux)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	release, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()

	before := stagingDirs(t)
	_, _, err = updater.Download(ctx, release)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a context.Canceled error, got: %v", err)
	}
	if after := stagingDirs(t); len(after) != len(before) {
		t.Fatalf("expected no leftover staging directories, before %v after %v", before, after)
	}
}

func TestDownloadRejectsUntrustedURLs(t *testing.T) {
	payload := []byte("this is the installer")
	fixture := newDownloadFixture(t, checksumLine(payload, testInstallerName), payload)

	cases := []struct {
		name    string
		mutate  func(release *UpdateRelease)
		message string
	}{
		{
			name: "foreign installer host",
			mutate: func(release *UpdateRelease) {
				release.InstallerURL = "https://evil.example.com/TaskView-Setup-1.3.0-windows-amd64.exe"
			},
			message: "not a GitHub host",
		},
		{
			name: "plain http installer",
			mutate: func(release *UpdateRelease) {
				release.InstallerURL = "http://github.com/DarlingCY/TaskView/releases/download/v1.3.0/" + testInstallerName
			},
			message: "https",
		},
		{
			name: "foreign checksum host",
			mutate: func(release *UpdateRelease) {
				release.ChecksumURL = "https://evil.example.com/SHA256SUMS"
			},
			message: "not a GitHub host",
		},
		{
			name: "installer name does not match the release",
			mutate: func(release *UpdateRelease) {
				release.InstallerName = "TaskView-Setup-9.9.9-windows-amd64.exe"
			},
			message: "installer name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := *fixture.release
			tc.mutate(&release)

			before := stagingDirs(t)
			_, _, err := fixture.updater.Download(context.Background(), &release)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("expected the error to mention %q, got: %v", tc.message, err)
			}
			if after := stagingDirs(t); len(after) != len(before) {
				t.Fatalf("expected no leftover staging directories, before %v after %v", before, after)
			}
		})
	}
}

func TestDownloadRejectsNilRelease(t *testing.T) {
	updater := &Updater{Client: http.DefaultClient, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}
	if _, _, err := updater.Download(context.Background(), nil); err == nil {
		t.Fatal("expected an error")
	}
}

// TestDownloadAcceptsVersionWithoutTagPrefix covers a release object that
// carries the bare version instead of the published tag. The download path
// still has to address the same release.
func TestDownloadAcceptsVersionWithoutTagPrefix(t *testing.T) {
	payload := []byte("this is the installer")
	fixture := newDownloadFixture(t, checksumLine(payload, testInstallerName), payload)

	release := *fixture.release
	release.Version = testVersion

	path, cleanup, err := fixture.updater.Download(context.Background(), &release)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	defer cleanup()
	if filepath.Base(path) != testInstallerName {
		t.Fatalf("expected the file %q, got %q", testInstallerName, filepath.Base(path))
	}
}
