//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// readWindowsProxyRegistry queries Windows Internet Settings from the current
// user's registry.
//
// Seam function overridable in tests. The Windows behaviour is unchanged.
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
