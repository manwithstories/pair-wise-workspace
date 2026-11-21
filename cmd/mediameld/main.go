// Command mediameld is the terminal acceptance entry point. It has no GUI
// dependencies: every capability is reached through flags and stdout.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"mediameld/internal/app"
	"mediameld/internal/model"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediameld demo --seed N --late-ms N --drift-ppm N --wrap-at N --vfr 24,25,23 --out DIR [--case normal|corrupt-klv]")
		return 1
	}
	opts, err := parseDemo(args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res, err := app.Run(opts, os.Stdout)
	if err != nil {
		code, _ := model.CodeOf(err)
		if code != "" {
			fmt.Fprintf(os.Stderr, "error_code=%s\n", code)
		}
		fmt.Fprintf(os.Stderr, "error=%s\n", err)
		// For the corrupt case the report already carries the exit code.
		if res.ExitCode != 0 {
			return res.ExitCode
		}
		return model.ExitCode(err)
	}
	if res.ExitCode != 0 {
		return res.ExitCode
	}
	return 0
}

func parseDemo(args []string) (app.Options, error) {
	opts := app.Options{Seed: 17, LateMS: 240, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if i+1 >= len(args) {
				return "", model.NewError(model.CodeUsage, "flag %s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--seed":
			v, err := val()
			if err != nil {
				return opts, err
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return opts, model.NewError(model.CodeUsage, "invalid --seed %q", v)
			}
			opts.Seed = n
		case "--late-ms":
			v, err := val()
			if err != nil {
				return opts, err
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return opts, model.NewError(model.CodeUsage, "invalid --late-ms %q", v)
			}
			opts.LateMS = n
		case "--drift-ppm":
			v, err := val()
			if err != nil {
				return opts, err
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return opts, model.NewError(model.CodeUsage, "invalid --drift-ppm %q", v)
			}
			opts.DriftPPM = n
		case "--wrap-at":
			v, err := val()
			if err != nil {
				return opts, err
			}
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return opts, model.NewError(model.CodeUsage, "invalid --wrap-at %q", v)
			}
			opts.WrapAt = uint32(n)
		case "--vfr":
			v, err := val()
			if err != nil {
				return opts, err
			}
			rates, err := parseVFR(v)
			if err != nil {
				return opts, err
			}
			opts.VFR = rates
		case "--out":
			v, err := val()
			if err != nil {
				return opts, err
			}
			opts.OutDir = v
		case "--case":
			v, err := val()
			if err != nil {
				return opts, err
			}
			opts.Case = v
		default:
			return opts, model.NewError(model.CodeUsage, "unknown flag %q", a)
		}
	}
	if opts.OutDir == "" {
		return opts, model.NewError(model.CodeUsage, "--out is required")
	}
	return opts, nil
}

func parseVFR(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || n <= 0 {
			return nil, model.NewError(model.CodeUsage, "invalid --vfr frame rate %q", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, model.NewError(model.CodeUsage, "--vfr requires at least one rate")
	}
	return out, nil
}
