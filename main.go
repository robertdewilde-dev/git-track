package main

import (
	"os"

	"github.com/robertdewilde-dev/git-track/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
