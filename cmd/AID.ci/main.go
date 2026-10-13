package main

import (
	"os"

	"aid/tools/ci"
)

func main() {
	os.Exit(ci.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
