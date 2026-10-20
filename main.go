// Command contexttax scans a repository for agent instruction files and
// serves a dashboard of their estimated preload cost.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	addr := flag.String("addr", ":7070", "dashboard listen address (rewritten to 127.0.0.1)")
	rootFlag := flag.String("root", "", "repository root to scan (defaults to the positional path, or .)")
	maxFiles := flag.Int("max-files", 2000, "maximum instruction files to read")
	maxWalk := flag.Int("max-walk", 50000, "maximum directory entries to walk")
	maxRead := flag.Int("max-read-kib", 256, "maximum kibibytes read from one instruction file")
	estimate := flag.String("estimate", "runes", "token estimate basis: runes or bytes, then divided by 4")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "contexttax scans instruction files agents load\nand serves a local dashboard of estimated preload tokens (an estimate: count ÷ 4).\n\nUsage:\n  contexttax [flags] [path]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *maxFiles < 1 || *maxWalk < 1 || *maxRead < 1 {
		fmt.Fprintln(os.Stderr, "contexttax: caps must be positive")
		os.Exit(2)
	}
	switch strings.ToLower(*estimate) {
	case "runes":
		estimateBytes = false
	case "bytes":
		estimateBytes = true
	default:
		fmt.Fprintln(os.Stderr, "contexttax: -estimate must be runes or bytes")
		os.Exit(2)
	}
	maxInstructionFiles = *maxFiles
	maxWalkEntries = *maxWalk
	maxReadBytes = int64(*maxRead) << 10

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
	if bound != *addr {
		fmt.Fprintf(os.Stderr, "contexttax: -addr %s is rewritten to %s. The dashboard listens only on 127.0.0.1.\n", *addr, bound)
	}

	fmt.Fprintf(os.Stderr, "contexttax scanning %s\nlistening on %s\n", root, bound)
	if err := Serve(root, bound); err != nil {
		fmt.Fprintf(os.Stderr, "contexttax: %v\n", err)
		os.Exit(1)
	}
}
