package main

import (
	"aid/tools/discover"
	"os"
)

func main() { os.Exit(discover.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
