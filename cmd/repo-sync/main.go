package main

import (
	"fmt"
	"os"

	"github.com/JorgeMuehlebach/repo-sync/internal/app"
)

var version = "dev"

func main() {
	application, err := app.New(version, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		os.Exit(1)
	}
	if err := application.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo-sync:", err)
		os.Exit(1)
	}
}
