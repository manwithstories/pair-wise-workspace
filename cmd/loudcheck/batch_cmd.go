package main

import (
	"strconv"

	"loudcheck/internal/batch"
	"loudcheck/internal/report"
)

func runBatch(args []string) error {
	flags := parseFlags(args)
	root, ok := flags["in"]
	if !ok {
		return &exitError{exitUsage, "batch requires --in <dir>"}
	}
	if profileName(flags) != "ebu-r128" {
		return &exitError{exitUsage, "only --profile ebu-r128 is supported"}
	}
	workers := 4
	if value, exists := flags["workers"]; exists {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return &exitError{exitUsage, "invalid --workers"}
		}
		workers = parsed
	}
	results, summary, err := batch.Run(root, workers, batch.EBUProfile())
	if err != nil {
		return &exitError{exitUsage, err.Error()}
	}
	report.PrintBatch(results, summary)
	if jsonPath, ok := flags["json"]; ok {
		if err := report.WriteJSON(jsonPath, "ebu-r128", results, summary); err != nil {
			return &exitError{exitUsage, err.Error()}
		}
	}
	if summary.ReadError > 0 {
		return &exitError{code: exitReadError}
	}
	if summary.Fail > 0 {
		return &exitError{code: exitFail}
	}
	return nil
}
