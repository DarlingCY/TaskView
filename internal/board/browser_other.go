//go:build !windows

package board

import (
	"os/exec"
	"runtime"
)

// OpenBrowser launches url in the default system browser without shell invocation.
//
// Unix has no rundll32, so the platform opener is used directly: "open" on
// macOS and "xdg-open" elsewhere. The process is never started through a shell.
func OpenBrowser(targetURL string) bool {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, targetURL)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() {
		_ = cmd.Wait()
	}()
	return true
}
