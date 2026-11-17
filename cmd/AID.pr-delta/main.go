package main

import (
	"os"

	prdelta "aid/tools/pr-delta"
)

func main() {
	os.Exit(prdelta.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
