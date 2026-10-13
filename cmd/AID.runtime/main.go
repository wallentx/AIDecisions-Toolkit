package main

import (
	"os"

	"aid/tools/runtime"
)

func main() {
	os.Exit(runtime.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
