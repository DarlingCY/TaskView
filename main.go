package main

import "os"

func main() {
	code := runMain(os.Args[1:])
	os.Exit(code)
}

func runMain(args []string) int {
	return RunCLI(args, nil, os.Stdout, os.Stderr)
}
