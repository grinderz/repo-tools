package main

import (
	"fmt"
	"os"

	"github.com/grinderz/repo-tools/internal/cli"
	"github.com/grinderz/repo-tools/internal/run"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, run.Red("error:"), err)
		os.Exit(1)
	}
}
