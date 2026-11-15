package main

import (
	"fmt"
	"os"
)

const (
	exitOK        = 0
	exitFail      = 2
	exitReadError = 1
	exitUsage     = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	var err error
	switch os.Args[1] {
	case "fixtures":
		err = runFixtures(os.Args[2:])
	case "analyze":
		err = runAnalyze(os.Args[2:])
	case "batch":
		err = runBatch(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		usage()
		os.Exit(exitUsage)
	}
	if err != nil {
		if coded, ok := err.(*exitError); ok {
			if coded.message != "" {
				fmt.Fprintln(os.Stderr, coded.message)
			}
			os.Exit(coded.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitUsage)
	}
}

type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }

func usage() {
	fmt.Fprintln(os.Stderr, "usage: loudcheck <fixtures|analyze|batch|verify> [flags]")
}

func parseFlags(args []string) map[string]string {
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		token := args[i]
		if len(token) < 2 || token[0] != '-' {
			continue
		}
		key := token
		for len(key) > 0 && key[0] == '-' {
			key = key[1:]
		}
		if i+1 < len(args) && (len(args[i+1]) == 0 || args[i+1][0] != '-') {
			flags[key] = args[i+1]
			i++
		} else {
			flags[key] = "true"
		}
	}
	return flags
}
