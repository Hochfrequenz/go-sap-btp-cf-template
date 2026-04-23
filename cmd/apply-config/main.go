// Command apply-config rewrites template-variable strings across the
// repository from the single source of truth in config.yml. Intended
// to be run once after forking this template:
//
//	go run ./cmd/apply-config --dry-run   # preview changes
//	go run ./cmd/apply-config             # apply to tree
//	go run ./cmd/apply-config --check     # exit 1 if tree drifts from config
//
// Idempotent: re-running with an unchanged config.yml is a no-op.
//
// The tool deliberately has zero dependencies on this module's
// internal/ packages, so it keeps working even while it's rewriting
// those packages' import paths.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	var (
		cfgPath = flag.String("config", "config.yml", "path to config.yml")
		root    = flag.String("root", ".", "repo root to rewrite files in")
		dryRun  = flag.Bool("dry-run", false, "print planned changes without writing")
		check   = flag.Bool("check", false, "exit 1 if any rewriter would change the tree (implies --dry-run)")
	)
	flag.Parse()
	if *check {
		*dryRun = true
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	result, err := Run(*root, cfg, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(result.Report())

	if *check && result.HasChanges() {
		os.Exit(1)
	}
}
