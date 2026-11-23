package main

import "flag"

// newFlagSet creates a flag set that returns errors instead of calling
// os.Exit, so the CLI can map every failure through the unified fault model
// and its exit-code contract.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}
