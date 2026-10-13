package main

import (
	"os"

	"aid/tools/pr"
)

func main() {
	os.Exit(pr.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
