package main

import (
	"os"

	"loudcheck/internal/batch"
	"loudcheck/internal/report"
)

func runAnalyze(args []string) error {
	flags := parseFlags(args)
	path, ok := flags["in"]
	if !ok {
		return &exitError{exitUsage, "analyze requires --in <file.wav>"}
	}
	if profileName(flags) != "ebu-r128" {
		return &exitError{exitUsage, "only --profile ebu-r128 is supported"}
	}
	result := batch.AnalyzeSingle(path, batch.EBUProfile())
	report.PrintAnalyze(result, batch.EBUProfile())
	if result.Status == batch.StatusReadError {
		if _, err := os.Stat(path); err != nil {
			return &exitError{exitUsage, err.Error()}
		}
		return &exitError{code: exitReadError}
	}
	if result.Status == batch.StatusFail {
		return &exitError{code: exitFail}
	}
	return nil
}

func profileName(flags map[string]string) string {
	if value, ok := flags["profile"]; ok {
		return value
	}
	return "ebu-r128"
}
