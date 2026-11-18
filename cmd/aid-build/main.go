// aid-build builds the suite locally; AID --build is the agent intake pipeline.
package main

import (
	"aid/internal/builder"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	root := flag.String("source", ".", "AID source checkout")
	out := flag.String("out", "dist", "native binary output directory")
	release := flag.String("release", os.Getenv("RELEASE_TAG"), "suite release tag or version (default: RELEASE_TAG or latest git tag)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := builder.BuildRelease(ctx, *root, *out, *release, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
