package main

// The Board HTTP layer lives in taskview/internal/board so it builds and is
// tested on Linux, macOS and Windows alike. This file is the thin forwarding
// layer the rest of the application (daemon.go, app.go, tray.go) and its tests
// keep calling, in the same style as db.go.
//
// What cannot move stays here: the embedded UI bundle (see assets.go), the
// build-time version (version.go) and the browser launcher, which the board
// package implements per platform.

import (
	"io/fs"
	"net/http"

	"taskview/internal/board"
)

// Board HTTP types.
type ServerOptions = board.ServerOptions
type RunningServer = board.RunningServer

// CreateApp returns the HTTP handler for TaskView. A nil staticFS means the
// embedded UI assets.
func CreateApp(store *Store, staticFS fs.FS) http.Handler {
	if staticFS == nil {
		staticFS = getPublicFS()
	}
	return board.CreateApp(store, staticFS, appVersion())
}

// StartServer launches TaskView HTTP server binding strictly to 127.0.0.1.
func StartServer(opts ServerOptions) (*RunningServer, error) {
	if opts.FS == nil {
		opts.FS = getPublicFS()
	}
	if opts.Version == "" {
		opts.Version = appVersion()
	}
	return board.StartServer(opts)
}

// OpenBrowser launches url in the default system browser without shell invocation.
func OpenBrowser(targetURL string) bool { return board.OpenBrowser(targetURL) }

func sendJSON(w http.ResponseWriter, status int, payload any) { board.SendJSON(w, status, payload) }

func sendError(w http.ResponseWriter, status int, msg string) { board.SendError(w, status, msg) }

func validateHost(rawHost string) (host, port string, err error) {
	return board.ValidateHost(rawHost)
}

func validateOrigin(origin string, reqHost, reqPort string) (bool, string) {
	return board.ValidateOrigin(origin, reqHost, reqPort)
}
