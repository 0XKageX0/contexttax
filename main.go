// Command contexttax scans a repository for agent instruction files and
// serves a dashboard of their estimated preload cost.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	addr := flag.String("addr", ":7070", "dashboard listen address")
	rootFlag := flag.String("root", "", "repository root to scan (defaults to the positional path, or .)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "contexttax scans instruction files agents load\nand serves a local dashboard of estimated preload tokens (Unicode code points ÷ 4).\n\nUsage:\n  contexttax [flags] [path]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	root := "."
	switch flag.NArg() {
	case 0:
	case 1:
		root = flag.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "contexttax: too many arguments")
		flag.Usage()
		os.Exit(2)
	}
	if *rootFlag != "" {
		root = *rootFlag
	}

	bound, err := listenAddr(*addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contexttax: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "contexttax scanning %s\nlistening on %s\n", root, bound)
	if err := Serve(root, bound); err != nil {
		fmt.Fprintf(os.Stderr, "contexttax: %v\n", err)
		os.Exit(1)
	}
}
