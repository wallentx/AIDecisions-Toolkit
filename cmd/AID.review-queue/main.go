package main

import (
	"os"

	reviewqueue "aid/tools/review-queue"
)

func main() {
	os.Exit(reviewqueue.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
