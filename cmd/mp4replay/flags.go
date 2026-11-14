package main

import (
	"flag"
	"strconv"

	"mp4replay/internal/report"
)

// flagSet builds a named sub-command flag set.
func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs
}

// parseRange parses an inclusive "start-end" range into a half-open
// [start, end+1) interval used by the slicing helpers.
func parseRange(s string) (int, int, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '-' && i != 0 {
			a, err1 := strconv.Atoi(s[:i])
			b, err2 := strconv.Atoi(s[i+1:])
			if err1 != nil || err2 != nil {
				break
			}
			return a, b + 1, nil
		}
	}
	return 0, 0, report.NewError(report.ExitArgs, "range",
		"expected start-end, got "+s)
}

func mustString(fs *flag.FlagSet, name string) (string, error) {
	f := fs.Lookup(name)
	if f == nil || f.Value.String() == "" {
		return "", report.NewError(report.ExitArgs, fs.Name(),
			"missing required --"+name)
	}
	return f.Value.String(), nil
}
