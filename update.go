// TaskView update detection and download.
//
// This file only *detects* and *downloads* a newer release. It never runs,
// launches or installs anything: the Windows build ships as an Inno Setup
// installer, so the caller receives the path of a verified installer and
// decides when to hand it over.
//
// Safety rules enforced here:
//
//   - Only stable releases are considered. The running version and the release
//     tag must both be strict vX.Y.Z versions. Development builds ("dev") and
//     downgrades are refused with a readable error instead of being silently
//     upgraded or rolled back.
//   - Only the DarlingCY/TaskView repository is queried, and only its "latest"
//     release. Drafts and pre-releases are refused.
//   - Release assets are addressed by their exact published names. Nothing in
//     this package executes a release asset.
//   - Every URL is HTTPS and restricted to GitHub's own hosts. Redirects are
//     followed a few times at most and only to those same hosts, and an asset
//     URL must point at this repository's release download path.
//   - No credentials are sent: no token, no cookie jar, no Authorization
//     header, and URLs carrying user info are rejected.
//   - Responses are bounded (1 MiB for the release metadata and the checksum
//     file, 200 MiB for the installer) and every request carries a timeout.
//   - The installer is downloaded into a fresh, empty staging directory and is
//     only returned after its SHA-256 matches the checksum published by the
//     same release.
//
// The checksum file travels with the installer in the same release, so a
// successful match shows the download is intact and is what the release
// published. It is not a signature and proves nothing about who published it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// updateOwner and updateRepo pin the only repository this updater talks to.
	updateOwner = "DarlingCY"
	updateRepo  = "TaskView"

	// defaultAPIURL is the GitHub endpoint for the latest published release.
	// It deliberately excludes drafts and pre-releases.
	defaultAPIURL = "https://api.github.com/repos/" + updateOwner + "/" + updateRepo + "/releases/latest"

	// checksumAssetName is the published name of the checksum list. It must be
	// present in a release before the installer can be trusted.
	checksumAssetName = "SHA256SUMS"

	// installerAssetPattern is the published name of the Windows installer.
	// The version is inserted without a leading "v": v1.3.0 publishes
	// TaskView-Setup-1.3.0-windows-amd64.exe.
	installerAssetPattern = "TaskView-Setup-%s-windows-amd64.exe"

	// stagingPrefix names the temporary directory a download is staged in.
	stagingPrefix = "taskview-update-"
)

// allowedGitHubHosts are the only hosts this updater talks to. Asset links
// start on github.com and are served from GitHub's asset hosts after a
// redirect.
var allowedGitHubHosts = map[string]bool{
	"api.github.com":                        true,
	"github.com":                            true,
	"release-assets.githubusercontent.com":  true,
	"objects.githubusercontent.com":         true,
	"github-releases.githubusercontent.com": true,
}

// Limits and timeouts. They are variables so that tests can lower them
// instead of moving hundreds of megabytes around.
var (
	// maxAPIBodyBytes bounds the release metadata response.
	maxAPIBodyBytes int64 = 1 << 20
	// maxChecksumBytes bounds the SHA256SUMS response.
	maxChecksumBytes int64 = 1 << 20
	// maxInstallerBytes bounds the installer download.
	maxInstallerBytes int64 = 200 << 20

	// apiTimeout bounds a metadata or checksum request.
	apiTimeout = 30 * time.Second
	// downloadTimeout bounds the installer download.
	downloadTimeout = 30 * time.Minute

	// maxRedirects bounds how many redirects a single request may follow.
	maxRedirects = 5
)

// ErrNoRelease is returned by Check when the repository has no published
// release yet and the API answers 404.
//
// It is deliberately not the same as "already up to date": an up to date
// installation returns (nil, nil), while ErrNoRelease means there was nothing
// to compare against.
var ErrNoRelease = errors.New("no published release found")

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// UpdateRelease describes a published release that is newer than the running
// build.
type UpdateRelease struct {
	// Version is the release tag exactly as published, e.g. "v1.3.0".
	Version string
	// PageURL is the human facing release page.
	PageURL string
	// Notes are the release notes in markdown.
	Notes string
	// InstallerURL is the direct download URL of the Windows installer.
	InstallerURL string
	// ChecksumURL is the direct download URL of the SHA256SUMS file.
	ChecksumURL string
	// InstallerName is the published asset name of the installer, e.g.
	// "TaskView-Setup-1.3.0-windows-amd64.exe".
	InstallerName string
}

// Updater checks for a newer release and downloads it.
//
// Client, CurrentVersion and APIURL are injection seams. Production code
// should use NewUpdater and leave APIURL alone; APIURL only exists so that
// tests can point the updater at a local server.
type Updater struct {
	// Client performs the requests. It may carry a proxy configuration, but
	// it is never given a token or any other credential.
	Client *http.Client
	// CurrentVersion is the version of the running build.
	CurrentVersion string
	// APIURL is the release metadata endpoint. It defaults to defaultAPIURL
	// and is only meant to be overridden by tests.
	APIURL string
}

// NewUpdater returns an updater for the running build.
func NewUpdater() *Updater {
	return &Updater{
		Client:         defaultUpdateClient(),
		CurrentVersion: appVersion(),
		APIURL:         defaultAPIURL,
	}
}

// Check looks for a newer stable release.
//
// It returns (nil, nil) when the running build is already up to date, and
// (nil, ErrNoRelease) when the repository has no published release yet.
// Every other failure is reported as an error that explains what went wrong.
func (u *Updater) Check(ctx context.Context) (*UpdateRelease, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	current, err := u.currentVersion()
	if err != nil {
		return nil, err
	}

	endpoint, err := u.apiEndpoint()
	if err != nil {
		return nil, err
	}

	release, err := u.fetchLatestRelease(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	if release.Draft {
		return nil, fmt.Errorf("release %q is a draft and cannot be installed", release.TagName)
	}
	if release.Prerelease {
		return nil, fmt.Errorf("release %q is a pre-release; only stable releases are supported", release.TagName)
	}

	latest, err := parseStableVersion(release.TagName)
	if err != nil {
		return nil, fmt.Errorf("cannot use release %q: only stable release versions (vX.Y.Z) are supported", release.TagName)
	}

	switch {
	case latest.compare(current) < 0:
		return nil, fmt.Errorf("release %s is older than the running version %s; downgrades are not supported",
			latest, current)
	case latest.compare(current) == 0:
		return nil, nil
	}

	installerName := fmt.Sprintf(installerAssetPattern, latest.String())

	installer, ok := findReleaseAsset(release.Assets, installerName)
	if !ok {
		return nil, fmt.Errorf("release %s does not publish the expected installer %q", release.TagName, installerName)
	}
	checksums, ok := findReleaseAsset(release.Assets, checksumAssetName)
	if !ok {
		return nil, fmt.Errorf("release %s does not publish %q", release.TagName, checksumAssetName)
	}
	if installer.Size > 0 && installer.Size > maxInstallerBytes {
		return nil, fmt.Errorf("release asset %q is %d bytes, which exceeds the %d byte limit",
			installerName, installer.Size, maxInstallerBytes)
	}
	if _, err := validateAssetURL(installer.URL, release.TagName, installerName); err != nil {
		return nil, fmt.Errorf("installer asset %q: %w", installerName, err)
	}
	if _, err := validateAssetURL(checksums.URL, release.TagName, checksumAssetName); err != nil {
		return nil, fmt.Errorf("checksum asset %q: %w", checksumAssetName, err)
	}

	return &UpdateRelease{
		Version:       release.TagName,
		PageURL:       releasePageURL(release),
		Notes:         release.Body,
		InstallerURL:  installer.URL,
		ChecksumURL:   checksums.URL,
		InstallerName: installerName,
	}, nil
}

// Download fetches the installer of a release into a fresh staging directory
// and returns the path of the verified file.
//
// The file is only returned once its SHA-256 matches the checksum published by
// the same release. The returned cleanup function removes the staging
// directory, including the installer; it is safe to call more than once and
// never fails loudly (on Windows it cannot delete an installer that is
// currently running, so the caller may have to keep the file around).
//
// On failure nothing is left behind: the staging directory is removed before
// the error is returned.
func (u *Updater) Download(ctx context.Context, rel *UpdateRelease) (string, func(), error) {
	noop := func() {}
	if ctx == nil {
		ctx = context.Background()
	}
	if rel == nil {
		return "", noop, errors.New("no release to download")
	}

	version, err := parseStableVersion(rel.Version)
	if err != nil {
		return "", noop, fmt.Errorf("cannot download release %q: only stable release versions (vX.Y.Z) are supported", rel.Version)
	}

	installerName := strings.TrimSpace(rel.InstallerName)
	if installerName == "" {
		installerName = fmt.Sprintf(installerAssetPattern, version.String())
	}
	if !validInstallerName(installerName, version) {
		return "", noop, fmt.Errorf("installer name %q is not the expected asset name %q",
			installerName, fmt.Sprintf(installerAssetPattern, version.String()))
	}
	if _, err := validateAssetURL(rel.InstallerURL, rel.Version, installerName); err != nil {
		return "", noop, fmt.Errorf("installer asset %q: %w", installerName, err)
	}
	if _, err := validateAssetURL(rel.ChecksumURL, rel.Version, checksumAssetName); err != nil {
		return "", noop, fmt.Errorf("checksum asset %q: %w", checksumAssetName, err)
	}

	expected, err := u.fetchInstallerChecksum(ctx, rel.ChecksumURL, installerName)
	if err != nil {
		return "", noop, err
	}

	staging, err := os.MkdirTemp("", stagingPrefix)
	if err != nil {
		return "", noop, fmt.Errorf("create staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	installerPath, err := u.downloadInstaller(ctx, rel.InstallerURL, staging, installerName, expected)
	if err != nil {
		cleanup()
		return "", noop, err
	}
	return installerPath, cleanup, nil
}

// ---------------------------------------------------------------------------
// Version handling
// ---------------------------------------------------------------------------

// stableVersionPattern matches a strict stable release version: major, minor
// and patch numbers without leading zeroes, with an optional "v" prefix.
var stableVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// installerNamePattern matches the published name of a Windows installer.
var installerNamePattern = regexp.MustCompile(`^TaskView-Setup-(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-windows-amd64\.exe$`)

// stableVersion is a parsed vX.Y.Z release version.
type stableVersion struct {
	major int
	minor int
	patch int
}

// String renders the version without the "v" prefix, which is the form used
// in the published asset names.
func (v stableVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// compare orders two versions: -1 when v is older, 0 when equal, 1 when newer.
func (v stableVersion) compare(other stableVersion) int {
	switch {
	case v.major != other.major:
		return compareInt(v.major, other.major)
	case v.minor != other.minor:
		return compareInt(v.minor, other.minor)
	case v.patch != other.patch:
		return compareInt(v.patch, other.patch)
	}
	return 0
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// parseStableVersion parses a strict vX.Y.Z version.
func parseStableVersion(raw string) (stableVersion, error) {
	trimmed := strings.TrimSpace(raw)
	match := stableVersionPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return stableVersion{}, fmt.Errorf("%q is not a stable release version (expected vX.Y.Z)", raw)
	}
	parts := make([]int, 3)
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return stableVersion{}, fmt.Errorf("%q has a version number that is out of range", raw)
		}
		parts[i] = n
	}
	return stableVersion{major: parts[0], minor: parts[1], patch: parts[2]}, nil
}

// currentVersion returns the version of the running build.
//
// A build that is not a stable release (a development build, or a version
// carrying a pre-release suffix) cannot be compared against a release, so it
// is refused with an explanation instead of being upgraded.
func (u *Updater) currentVersion() (stableVersion, error) {
	raw := strings.TrimSpace(u.CurrentVersion)
	if raw == "" {
		raw = strings.TrimSpace(appVersion())
	}
	version, err := parseStableVersion(raw)
	if err != nil {
		return stableVersion{}, fmt.Errorf("cannot check for updates: the running version %q is not a stable release "+
			"version (expected vX.Y.Z); automatic updates are disabled for development builds", raw)
	}
	return version, nil
}

// validInstallerName reports whether name is the installer asset expected for
// the given release version.
func validInstallerName(name string, version stableVersion) bool {
	if !installerNamePattern.MatchString(name) {
		return false
	}
	return name == fmt.Sprintf(installerAssetPattern, version.String())
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

// defaultUpdateClient builds the client used in production.
//
// It honours the environment and Windows system proxy settings but never carries
// a token: there is no Authorization header, no cookie jar and no credentials in
// any URL.
func defaultUpdateClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = resolveUpdateProxy
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.ExpectContinueTimeout = 5 * time.Second
	transport.MaxIdleConnsPerHost = 4
	// No client wide timeout: every request carries its own context deadline,
	// and a large installer legitimately takes longer than an API call.
	return &http.Client{Transport: transport}
}

// resolveUpdateProxy determines the proxy to use for updater requests.
//
// Precedence:
//  1. Environment variables (HTTPS_PROXY, HTTP_PROXY, ALL_PROXY, NO_PROXY).
//  2. Windows Internet Settings (HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings).
//
// In all cases, local loopback (127.0.0.1, localhost, ::1) is never proxied.
func resolveUpdateProxy(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}

	// Always bypass loopback regardless of source.
	if isLoopbackHost(req.URL.Hostname()) {
		return nil, nil
	}

	// 1. Try environment variables first.
	if proxyURL, err := envProxyFunc(req); err != nil {
		return nil, err
	} else if proxyURL != nil {
		return proxyURL, nil
	}

	// 2. Fallback to Windows system proxy.
	return resolveWindowsProxy(req.URL)
}

// envProxyFunc resolves environment proxy settings for a request.
// Seam function overridable in tests.
var envProxyFunc = func(req *http.Request) (*url.URL, error) {
	scheme := strings.ToLower(req.URL.Scheme)
	var proxyStr string
	if scheme == "https" {
		proxyStr = getEnvAny("HTTPS_PROXY", "https_proxy")
	}
	if proxyStr == "" {
		proxyStr = getEnvAny("HTTP_PROXY", "http_proxy")
	}
	if proxyStr == "" {
		proxyStr = getEnvAny("ALL_PROXY", "all_proxy")
	}
	proxyStr = strings.TrimSpace(proxyStr)
	if proxyStr == "" {
		return nil, nil
	}

	noProxy := getEnvAny("NO_PROXY", "no_proxy")
	if shouldOverrideProxy(req.URL.Hostname(), noProxy) {
		return nil, nil
	}

	if !strings.Contains(proxyStr, "://") {
		proxyStr = "http://" + proxyStr
	}
	return url.Parse(proxyStr)
}

func getEnvAny(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// readWindowsProxyRegistry queries Windows Internet Settings from the current user's registry.
// Seam function overridable in tests.
var readWindowsProxyRegistry = func() (enabled bool, server string, override string, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return false, "", "", err
	}
	defer k.Close()

	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return false, "", "", nil
	}

	server, _, err = k.GetStringValue("ProxyServer")
	if err != nil || strings.TrimSpace(server) == "" {
		return false, "", "", nil
	}

	override, _, _ = k.GetStringValue("ProxyOverride")
	return true, server, override, nil
}

func resolveWindowsProxy(targetURL *url.URL) (*url.URL, error) {
	enabled, server, override, err := readWindowsProxyRegistry()
	if err != nil || !enabled {
		return nil, nil
	}
	return parseWindowsProxy(server, override, targetURL)
}

func parseWindowsProxy(serverStr, overrideStr string, targetURL *url.URL) (*url.URL, error) {
	if targetURL == nil {
		return nil, nil
	}

	host := targetURL.Hostname()
	if isLoopbackHost(host) {
		return nil, nil
	}

	if shouldOverrideProxy(host, overrideStr) {
		return nil, nil
	}

	serverStr = strings.TrimSpace(serverStr)
	if serverStr == "" {
		return nil, nil
	}

	var proxyAddress string
	if strings.Contains(serverStr, "=") {
		// Multi-protocol format, e.g. "http=127.0.0.1:8080;https=127.0.0.1:8443;socks=..."
		scheme := strings.ToLower(targetURL.Scheme)
		if scheme == "" {
			scheme = "https"
		}
		protoMap := make(map[string]string)
		for _, part := range strings.Split(serverStr, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			eq := strings.IndexByte(part, '=')
			if eq > 0 {
				pProto := strings.ToLower(strings.TrimSpace(part[:eq]))
				pAddr := strings.TrimSpace(part[eq+1:])
				protoMap[pProto] = pAddr
			}
		}

		if addr, ok := protoMap[scheme]; ok && addr != "" {
			proxyAddress = addr
		} else if addr, ok := protoMap["socks"]; ok && addr != "" {
			if !strings.Contains(addr, "://") {
				proxyAddress = "socks5://" + addr
			} else {
				proxyAddress = addr
			}
		} else {
			return nil, nil
		}
	} else {
		// Simple format, applies to all protocols.
		proxyAddress = serverStr
	}

	if !strings.Contains(proxyAddress, "://") {
		proxyAddress = "http://" + proxyAddress
	}

	return url.Parse(proxyAddress)
}

func shouldOverrideProxy(host, overrideStr string) bool {
	overrideStr = strings.TrimSpace(overrideStr)
	if overrideStr == "" {
		return false
	}

	isLocalHost := !strings.Contains(host, ".") && net.ParseIP(host) == nil

	entries := strings.FieldsFunc(overrideStr, func(r rune) bool {
		return r == ';' || r == ' ' || r == '\t'
	})

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.EqualFold(entry, "<local>") {
			if isLocalHost {
				return true
			}
			continue
		}
		if entry == "*" {
			return true
		}
		if strings.HasPrefix(entry, "*.") {
			suffix := entry[1:] // ".example.com"
			if strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix)) {
				return true
			}
		} else if strings.HasPrefix(entry, "*") {
			suffix := entry[1:]
			if strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix)) {
				return true
			}
		} else if strings.HasSuffix(entry, "*") {
			prefix := entry[:len(entry)-1]
			if strings.HasPrefix(strings.ToLower(host), strings.ToLower(prefix)) {
				return true
			}
		} else if strings.EqualFold(host, entry) {
			return true
		}
	}
	return false
}

// client returns a copy of the configured client with the redirect policy
// applied. The caller's client is never modified.
func (u *Updater) client() *http.Client {
	base := u.Client
	if base == nil {
		base = defaultUpdateClient()
	}
	clone := *base
	previous := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("too many redirects (limit is %d)", maxRedirects)
		}
		if err := validateGitHubURL(req.URL); err != nil {
			return fmt.Errorf("refusing redirect: %w", err)
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
	return &clone
}

// apiEndpoint returns the validated release metadata endpoint.
//
// The default endpoint is fixed. An overridden endpoint is only accepted when
// it is an HTTPS URL on one of GitHub's hosts, or when it points at the local
// machine, which is the seam tests use to serve a fake release.
func (u *Updater) apiEndpoint() (string, error) {
	raw := strings.TrimSpace(u.APIURL)
	if raw == "" {
		raw = defaultAPIURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid update API endpoint %q: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid update API endpoint %q: no host", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("invalid update API endpoint %q: it must not carry credentials", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid update API endpoint %q: it must not carry a query or fragment", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	if isLoopbackHost(host) {
		return raw, nil
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid update API endpoint %q: it must use https", raw)
	}
	if !allowedGitHubHosts[host] {
		return "", fmt.Errorf("invalid update API endpoint %q: %q is not a GitHub host", raw, host)
	}
	if parsed.Path != "/repos/"+updateOwner+"/"+updateRepo+"/releases/latest" {
		return "", fmt.Errorf("invalid update API endpoint %q: only the latest release of %s/%s may be queried",
			raw, updateOwner, updateRepo)
	}
	return raw, nil
}

// validateGitHubURL checks that a URL is an HTTPS URL on one of GitHub's
// hosts and carries no credentials.
func validateGitHubURL(target *url.URL) error {
	if target == nil {
		return errors.New("missing URL")
	}
	if target.User != nil {
		return errors.New("the URL must not carry credentials")
	}
	if target.Scheme != "https" {
		return fmt.Errorf("the URL must use https, got %q", target.Scheme)
	}
	host := strings.ToLower(target.Hostname())
	if !allowedGitHubHosts[host] {
		return fmt.Errorf("%q is not a GitHub host", host)
	}
	return nil
}

// validateAssetURL checks that a release asset URL addresses exactly the
// expected file of the expected release of this repository.
func validateAssetURL(raw string, tag string, assetName string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("the release does not provide a download URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid download URL %q: %w", raw, err)
	}
	if err := validateGitHubURL(parsed); err != nil {
		return nil, fmt.Errorf("invalid download URL %q: %w", raw, err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid download URL %q: it must not carry a query or fragment", raw)
	}
	expected := acceptableAssetPaths(tag, assetName)
	for _, candidate := range expected {
		if parsed.EscapedPath() == candidate {
			return parsed, nil
		}
	}
	return nil, fmt.Errorf("invalid download URL %q: it must point at %q", raw, expected[0])
}

// acceptableAssetPaths lists the download paths a release asset may use: the
// tag as published and the same tag without its "v" prefix. Nothing else is
// accepted, so an asset always belongs to this release of this repository.
func acceptableAssetPaths(tag string, assetName string) []string {
	tags := []string{tag}
	trimmed := strings.TrimPrefix(tag, "v")
	if trimmed != tag {
		tags = append(tags, trimmed)
	} else {
		tags = append(tags, "v"+tag)
	}
	paths := make([]string, 0, len(tags))
	for _, candidate := range tags {
		paths = append(paths, "/"+updateOwner+"/"+updateRepo+"/releases/download/"+
			url.PathEscape(candidate)+"/"+url.PathEscape(assetName))
	}
	return paths
}

// isLoopbackHost reports whether host addresses the local machine. Such
// endpoints are only reachable by the local user and exist so tests can serve
// a fake release. Local loopback also bypasses proxying.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if strings.HasPrefix(host, "127.") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// Release metadata
// ---------------------------------------------------------------------------

// githubRelease is the part of the GitHub release payload this updater uses.
type githubRelease struct {
	TagName    string        `json:"tag_name"`
	HTMLURL    string        `json:"html_url"`
	Body       string        `json:"body"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []githubAsset `json:"assets"`
}

// githubAsset is a single published release asset.
type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// findReleaseAsset returns the asset published under exactly this name.
func findReleaseAsset(assets []githubAsset, name string) (githubAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return githubAsset{}, false
}

// releasePageURL returns the human facing page of a release. The page from the
// API is only used when it points at this repository's release pages.
func releasePageURL(release *githubRelease) string {
	fallback := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", updateOwner, updateRepo, release.TagName)
	if strings.TrimSpace(release.HTMLURL) == "" {
		return fallback
	}
	parsed, err := url.Parse(release.HTMLURL)
	if err != nil {
		return fallback
	}
	if err := validateGitHubURL(parsed); err != nil {
		return fallback
	}
	if !strings.HasPrefix(parsed.Path, "/"+updateOwner+"/"+updateRepo+"/releases/") {
		return fallback
	}
	return release.HTMLURL
}

// fetchLatestRelease retrieves the latest release metadata.
func (u *Updater) fetchLatestRelease(ctx context.Context, endpoint string) (*githubRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build release request: %w", err)
	}
	u.setRequestHeaders(req)

	resp, err := u.client().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("query release metadata: %w", ctxErr)
		}
		return nil, fmt.Errorf("query release metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query release metadata: unexpected status %s%s", resp.Status, apiErrorDetail(resp.Body))
	}
	if resp.ContentLength > maxAPIBodyBytes {
		return nil, fmt.Errorf("query release metadata: the response is larger than the %d byte limit", maxAPIBodyBytes)
	}

	body, err := readBounded(resp.Body, maxAPIBodyBytes, "the release metadata")
	if err != nil {
		return nil, fmt.Errorf("query release metadata: %w", err)
	}

	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parse release metadata: %w", err)
	}
	return &release, nil
}

// setRequestHeaders adds the headers GitHub asks for. No credential is ever
// attached.
func (u *Updater) setRequestHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", u.userAgent())
}

// setAssetHeaders adds the headers used for release asset downloads.
func (u *Updater) setAssetHeaders(req *http.Request) {
	u.setRequestHeaders(req)
	req.Header.Set("Accept", "application/octet-stream")
}

// userAgent identifies the application to GitHub.
func (u *Updater) userAgent() string {
	raw := strings.TrimSpace(u.CurrentVersion)
	if raw == "" {
		raw = strings.TrimSpace(appVersion())
	}
	if version, err := parseStableVersion(raw); err == nil {
		return "TaskView/" + version.String()
	}
	return "TaskView/" + strings.TrimPrefix(raw, "v")
}

// apiErrorDetail turns an error response into a short readable suffix.
func apiErrorDetail(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil || len(data) == 0 {
		return ""
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && payload.Message != "" {
		return ": " + payload.Message
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return ": " + text
}

// ---------------------------------------------------------------------------
// Checksums
// ---------------------------------------------------------------------------

// checksumEntry is one line of a SHA256SUMS file.
type checksumEntry struct {
	sum  string
	name string
}

// fetchInstallerChecksum downloads the checksum list and returns the single
// checksum published for the given installer.
//
// The list must mention the installer exactly once: a missing entry means the
// release cannot be verified, and several entries with the same name mean
// there is no way to tell which one is meant.
func (u *Updater) fetchInstallerChecksum(ctx context.Context, checksumURL string, installerName string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return "", fmt.Errorf("build checksum request: %w", err)
	}
	u.setAssetHeaders(req)

	resp, err := u.client().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("download checksum file: %w", ctxErr)
		}
		return "", fmt.Errorf("download checksum file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksum file: unexpected status %s%s", resp.Status, apiErrorDetail(resp.Body))
	}
	if resp.ContentLength > maxChecksumBytes {
		return "", fmt.Errorf("download checksum file: the response is larger than the %d byte limit", maxChecksumBytes)
	}

	body, err := readBounded(resp.Body, maxChecksumBytes, "the checksum file")
	if err != nil {
		return "", fmt.Errorf("download checksum file: %w", err)
	}

	entries := parseChecksumFile(body)
	var matches []checksumEntry
	for _, entry := range entries {
		if entry.matches(installerName) {
			matches = append(matches, entry)
		}
	}
	switch {
	case len(matches) == 0:
		return "", fmt.Errorf("%s does not list %q, the download cannot be verified", checksumAssetName, installerName)
	case len(matches) > 1:
		return "", fmt.Errorf("%s lists %q %d times, the download cannot be verified", checksumAssetName, installerName, len(matches))
	}
	return matches[0].sum, nil
}

// matches reports whether the entry refers to the given installer name. The
// comparison uses the file name only, so a checksum file written from a
// directory ("dist/TaskView-Setup-1.3.0-windows-amd64.exe") still matches.
func (e checksumEntry) matches(installerName string) bool {
	name := e.name
	if index := strings.LastIndexAny(name, `\/`); index >= 0 {
		name = name[index+1:]
	}
	return name == installerName
}

// parseChecksumFile reads a SHA256SUMS file. Lines that do not carry a
// SHA-256 sum are ignored.
func parseChecksumFile(data []byte) []checksumEntry {
	var entries []checksumEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if !isSHA256Hex(sum) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		entries = append(entries, checksumEntry{sum: sum, name: name})
	}
	return entries
}

// isSHA256Hex reports whether s is a 64 character lowercase hex string.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// downloadInstaller stores the installer in the staging directory and returns
// the path of the verified file.
//
// The download is written to a temporary name first and only renamed to the
// final name once the checksum matched, so the final name never exists for an
// unverified file.
func (u *Updater) downloadInstaller(ctx context.Context, rawURL string, staging string, installerName string, expectedSum string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build installer request: %w", err)
	}
	u.setAssetHeaders(req)

	resp, err := u.client().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("download installer: %w", ctxErr)
		}
		return "", fmt.Errorf("download installer: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download installer: unexpected status %s%s", resp.Status, apiErrorDetail(resp.Body))
	}
	if resp.ContentLength > maxInstallerBytes {
		return "", fmt.Errorf("download installer: the file is %d bytes, which exceeds the %d byte limit",
			resp.ContentLength, maxInstallerBytes)
	}

	partial := filepath.Join(staging, installerName+".part")
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", partial, err)
	}
	removePartial := true
	defer func() {
		if removePartial {
			_ = os.Remove(partial)
		}
	}()
	// The handle is closed before the removal above runs: Windows refuses to
	// delete a file that is still open.
	defer file.Close()

	hasher := sha256.New()
	written, err := copyBounded(io.MultiWriter(file, hasher), resp.Body, maxInstallerBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("download installer: %w", ctxErr)
		}
		return "", fmt.Errorf("download installer: %w", err)
	}
	if written == 0 {
		return "", errors.New("download installer: the server sent an empty file")
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("flush %s: %w", partial, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", partial, err)
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(sum, expectedSum) {
		return "", fmt.Errorf("checksum mismatch for %q: the release publishes %s but the download is %s",
			installerName, strings.ToLower(expectedSum), sum)
	}

	final := filepath.Join(staging, installerName)
	if err := os.Rename(partial, final); err != nil {
		return "", fmt.Errorf("store installer as %s: %w", final, err)
	}
	removePartial = false
	return final, nil
}

// ---------------------------------------------------------------------------
// Bounded reads
// ---------------------------------------------------------------------------

// readBounded reads at most limit bytes and fails when more data is offered.
func readBounded(r io.Reader, limit int64, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d byte limit", what, limit)
	}
	return data, nil
}

// copyBounded copies at most limit bytes and fails when more data is offered.
func copyBounded(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, fmt.Errorf("the download is larger than the %d byte limit", limit)
	}
	return written, nil
}
