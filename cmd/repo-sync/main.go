package main

import (
	"fmt"
	"os"

	"github.com/JorgeMuehlebach/repo-sync/internal/app"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitexec"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	defer gitexec.CleanupProcessSnapshots()
	application, err := app.New(version, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		return 1
	}
	if err := application.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		return 1
	}
	return 0
}
