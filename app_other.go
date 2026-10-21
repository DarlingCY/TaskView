//go:build !windows

package main

import (
	"context"
	"errors"
)

func installedApplicationPath() (string, error) {
	return "", errors.New("the installed application is supported only on Windows")
}
func launchUpdateInstaller(context.Context, string) error {
	return errors.New("installation updates are supported only on Windows")
}
