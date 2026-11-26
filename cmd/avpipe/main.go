// Command avpipe is the terminal entry point for the low-latency replay
// pipeline. Its only subcommand is "demo", which generates a fully local
// fixture asset, injects transport faults and runs the complete flow.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"avpipe/internal/demo"
	"avpipe/internal/media"
)

// Exit codes follow the acceptance contract.
const (
	exitOK      = 0
	exitArgs    = 2
	exitAsset   = 3
	exitIndex   = 4
	exitRuntime = 1
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: avpipe demo --out DIR [flags]")
		os.Exit(exitArgs)
	}
	opt, err := parseDemo(os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "argument error:", err)
		os.Exit(exitArgs)
	}
	rep, err := demo.Run(opt)
	if rep != nil {
		demo.PrintReport(os.Stdout, rep, opt)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline error:", err)
		os.Exit(mapExit(err))
	}
	if rep != nil {
		if failures := demo.Validate(rep, opt); len(failures) > 0 {
			for _, f := range failures {
				fmt.Fprintln(os.Stderr, "acceptance failure:", f)
			}
			os.Exit(exitAsset)
		}
	}
	fmt.Println("demo completed: all acceptance checks passed")
	os.Exit(exitOK)
}

func parseDemo(args []string) (demo.Options, error) {
	opt := demo.Options{
		FPS: 30, Segment: "1s", Faults: "mixed",
		InterruptAfter: 0,
	}
	needValue := map[string]bool{
		"--out": true, "--frames": true, "--fps": true, "--segment": true,
		"--targets": true, "--seed": true, "--faults": true,
		"--interrupt-after": true,
	}
	boolFlags := map[string]bool{"--resume": true}
	for i := 0; i < len(args); i++ {
		token := args[i]
		key, inlineVal, hasInline := strings.Cut(token, "=")
		switch key {
		case "--out":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			opt.OutDir = v
		case "--frames":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return opt, fmt.Errorf("--frames must be an integer")
			}
			opt.Frames = uint32(n)
		case "--fps":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return opt, err
			}
			opt.FPS = uint32(n)
		case "--segment":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			opt.Segment = v
		case "--targets":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			targets, err := parseTargets(v)
			if err != nil {
				return opt, err
			}
			opt.Targets = targets
		case "--seed":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return opt, err
			}
			opt.Seed = n
		case "--faults":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			opt.Faults = v
		case "--interrupt-after":
			v, err := nextValue(args, &i, hasInline, inlineVal, key)
			if err != nil {
				return opt, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return opt, err
			}
			opt.InterruptAfter = n
		case "--resume":
			opt.Resume = true
		default:
			if boolFlags[key] || needValue[key] {
				continue
			}
			return opt, fmt.Errorf("unknown flag %q", token)
		}
	}
	if opt.OutDir == "" {
		return opt, fmt.Errorf("--out is required")
	}
	if opt.Frames == 0 {
		return opt, fmt.Errorf("--frames is required")
	}
	if len(opt.Targets) == 0 {
		return opt, fmt.Errorf("--targets is required")
	}
	return opt, nil
}

func nextValue(args []string, i *int, hasInline bool, inline, key string) (string, error) {
	if hasInline {
		return inline, nil
	}
	if *i+1 >= len(args) {
		return "", fmt.Errorf("%s requires a value", key)
	}
	*i++
	return args[*i], nil
}

func parseTargets(v string) ([]int, error) {
	parts := strings.Split(v, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimSuffix(p, "k")
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid target %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func mapExit(err error) int {
	var me *media.Error
	if errors.As(err, &me) {
		switch me.Class {
		case media.ClassConfig:
			return exitArgs
		case media.ClassValidation, media.ClassCorrupt, media.ClassDuplicate,
			media.ClassLoss, media.ClassClock, media.ClassEncode:
			return exitAsset
		case media.ClassIndex:
			return exitIndex
		}
	}
	return exitRuntime
}
