package main

import "errors"

// Stubs replaced by real subcommand implementations in later commits.

var errNotImplemented = errors.New("subcommand not implemented yet")

func cmdDemo(args []string) (int, error)   { return exitUsage, errNotImplemented }
func cmdVerify(args []string) (int, error) { return exitUsage, errNotImplemented }
func cmdQuery(args []string) (int, error)  { return exitUsage, errNotImplemented }
