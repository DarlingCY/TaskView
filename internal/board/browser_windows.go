//go:build windows

package board

import "os/exec"

// OpenBrowser launches url in the default system browser without shell invocation.
//
// The Windows behaviour is unchanged: rundll32 hands the URL to the registered
// protocol handler.
func OpenBrowser(targetURL string) bool {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() {
		_ = cmd.Wait()
	}()
	return true
}
