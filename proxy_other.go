//go:build !windows

package main

// readWindowsProxyRegistry has no registry to read off Windows, so the Windows
// system-proxy fallback never applies. The environment proxy variables
// (HTTPS_PROXY, HTTP_PROXY, ALL_PROXY) are unaffected.
var readWindowsProxyRegistry = func() (enabled bool, server string, override string, err error) {
	return false, "", "", nil
}
