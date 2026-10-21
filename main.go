package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(args []string) int {
	if err := runApplication(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitErr *applicationExitError
		if errors.As(err, &exitErr) {
			return exitErr.Code
		}
		internal, background := false, false
		for _, arg := range args {
			internal = internal || strings.HasPrefix(arg, "--internal-")
			background = background || arg == "--background"
		}
		if internal {
			// The installer reads the exit code and the transaction file.
			return 1
		}
		if background {
			// An automatic start has no window to show a dialog in; the log is
			// the only place the user can find out what went wrong.
			logStartupFailure(err)
			return 1
		}
		platformNotify("TaskView", err.Error())
		return 1
	}
	return 0
}

// logStartupFailure appends a start-up error to the server log, next to the
// messages the managed server writes itself.
func logStartupFailure(err error) {
	dir, dirErr := DataDir()
	if dirErr != nil {
		return
	}
	f, openErr := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if openErr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[taskview] start failed: %v\n", err)
}
