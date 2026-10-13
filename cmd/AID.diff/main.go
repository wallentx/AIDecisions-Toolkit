package main

import (
	"os"

	"aid/tools/diff"
)

func main() {
	os.Exit(diff.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
