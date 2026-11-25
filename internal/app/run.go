package app

import (
	"context"
	"fmt"
	"io"

	"mediaarchive/internal/fault"
)

// Run 执行 CLI 并返回进程退出码；所有终端输出集中在这里，便于测试。
func Run(ctx context.Context, args []string, stdout io.Writer) int {
	cfg, perr := ParseArgs(args)
	if perr != nil {
		fmt.Fprintf(stdout, "status=usage_error\n")
		fmt.Fprintf(stdout, "error_kind=%s\n", perr.Kind)
		fmt.Fprintf(stdout, "error=%s\n", perr.Message)
		fmt.Fprintf(stdout, "exit_code=%d\n", fault.ExitUsage)
		return fault.ExitUsage
	}

	res, err := execute(ctx, cfg)
	if err != nil {
		code := fault.ExitCode(err)
		fmt.Fprintf(stdout, "status=failed\n")
		printCommon(stdout, res)
		if err.Location != nil {
			fmt.Fprintf(stdout, "error_source=%s\n", err.Location.Source)
			fmt.Fprintf(stdout, "error_offset=%d\n", err.Location.Offset)
		}
		fmt.Fprintf(stdout, "error_kind=%s\n", err.Kind)
		fmt.Fprintf(stdout, "error=%s\n", err.Message)
		fmt.Fprintf(stdout, "exit_code=%d\n", code)
		return code
	}

	printSuccess(stdout, res)
	return res.ExitCode
}

func printCommon(w io.Writer, r *Result) {
	if r == nil {
		return
	}
	fmt.Fprintf(w, "scenario=%s\n", r.Config.Scenario)
	fmt.Fprintf(w, "tracks=%d\n", r.Tracks)
	fmt.Fprintf(w, "events=%d\n", r.Events)
	fmt.Fprintf(w, "expected_packet=%d\n", r.ExpectedPacket)
	fmt.Fprintf(w, "located_packet=%d\n", r.LocatedPacket)
	fmt.Fprintf(w, "offset_error=%d\n", r.OffsetError)
	fmt.Fprintf(w, "checkpoint_events=%d\n", r.CheckpointEvents)
	fmt.Fprintf(w, "resumed_events=%d\n", r.ResumedEvents)
	fmt.Fprintf(w, "late_replays=%d\n", r.LateReplays)
	fmt.Fprintf(w, "frames_exported=%d\n", r.FramesExported)
	if r.FramesHash != "" {
		fmt.Fprintf(w, "frames_sha256=%s\n", r.FramesHash)
	}
	if r.ManifestHash != "" {
		fmt.Fprintf(w, "manifest_sha256=%s\n", r.ManifestHash)
	}
	if r.StateHash != "" {
		fmt.Fprintf(w, "state_sha256=%s\n", r.StateHash)
	}
	fmt.Fprintf(w, "params_hash=%s\n", r.ParamsHash)
	fmt.Fprintf(w, "recovery_ms=%d\n", r.RecoveryMS)
}

func printSuccess(w io.Writer, r *Result) {
	if r.ExitCode == fault.ExitCheckpoint {
		fmt.Fprintf(w, "status=checkpoint_saved\n")
		printCommon(w, r)
		fmt.Fprintf(w, "checkpoint_path=%s\n", outputPaths(r.Config.Out, r.Config.Compare).checkpoint)
		fmt.Fprintf(w, "exit_code=%d\n", fault.ExitCheckpoint)
		return
	}

	fmt.Fprintf(w, "status=ok\n")
	printCommon(w, r)
	fmt.Fprintf(w, "expected_offset=%d\n", r.DamageOffset)
	fmt.Fprintf(w, "located_offset=%d\n", r.LocatedOffset)
	if r.Config.Compare != "" {
		if r.CompareMatch {
			fmt.Fprintf(w, "compare=match\n")
		} else {
			fmt.Fprintf(w, "compare=mismatch\n")
		}
	}
	fmt.Fprintf(w, "exit_code=0\n")
}
