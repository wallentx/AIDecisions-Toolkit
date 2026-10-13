package main

import (
	"aid/internal/launcher"
	"os"
)

func main() { os.Exit(launcher.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
