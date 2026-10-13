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
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := builder.Build(ctx, *root, *out, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
