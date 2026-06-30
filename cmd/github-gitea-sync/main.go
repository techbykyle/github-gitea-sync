package main

import (
	"os"

	"github-gitea-sync/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}
