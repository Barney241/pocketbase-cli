package main

import (
	"os"

	"github.com/Barney241/pocketbase-cli/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
