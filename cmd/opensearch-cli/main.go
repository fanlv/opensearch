package main

import (
	"os"

	"github.com/fanlv/opensearch/internal/cli"
)

// version 由 Makefile 经 -ldflags "-X main.version=..." 注入。
var version = "0.1.0"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
