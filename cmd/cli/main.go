package main

import (
	"flag"
	"fmt"
	"os"
)

const Version = "0.0.1"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "ping":
		cmdPing()
	case "log":
		cmdLog()
	case "slack":
		cmdSlack()
	case "--version":
		fmt.Printf("clusage-cli v%s\n", Version)
	case "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `clusage-cli v%s

Usage:
  clusage-cli ping
  clusage-cli log [--from-hook | --input-tokens N --output-tokens N ...]
  clusage-cli slack [--format json|release-bool|fraction]

`, Version)
}

func cmdPing() {
	// Placeholder: will implement in Phase 2
	fmt.Println("Placeholder: ping subcommand")
}

func cmdLog() {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	fromHook := fs.Bool("from-hook", false, "read hook payload from stdin")
	fs.Parse(os.Args[2:])

	if *fromHook {
		// Placeholder: Mode B (hook) will be implemented in Phase 3
		fmt.Println("Placeholder: log --from-hook subcommand")
	} else {
		// Placeholder: Mode A (flags) will be implemented in Phase 2
		fmt.Println("Placeholder: log with flags subcommand")
	}
}

func cmdSlack() {
	fs := flag.NewFlagSet("slack", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json|release-bool|fraction")
	fs.Parse(os.Args[2:])

	// Placeholder: will implement in Phase 5
	fmt.Printf("Placeholder: slack --format %s subcommand\n", *format)
}
