package main

// commandNames lists every supported sub-command. Handlers register themselves
// through init() in the per-command files.
var commandNames = []string{
	"probe", "index", "seek", "export", "hash", "gen", "verify",
}
