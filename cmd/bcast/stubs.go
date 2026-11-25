package main

import "bcast/internal/fault"

func runResume(args []string) error {
	return fault.E(fault.Internal, "cli", "resume", errNotImplemented)
}

func runVerify(args []string) error {
	return fault.E(fault.Internal, "cli", "verify", errNotImplemented)
}

var errNotImplemented = stringError("not implemented")

type stringError string

func (e stringError) Error() string { return string(e) }
