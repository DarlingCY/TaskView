package main

import (
	"embed"
	"io/fs"
)

//go:embed src/public/*
var embeddedAssets embed.FS

//go:embed resources/taskview.ico
var trayIcon []byte

// getPublicFS returns an fs.FS rooted at src/public.
func getPublicFS() fs.FS {
	sub, err := fs.Sub(embeddedAssets, "src/public")
	if err != nil {
		panic(err)
	}
	return sub
}
