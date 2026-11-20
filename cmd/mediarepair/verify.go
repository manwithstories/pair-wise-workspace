package main

import (
	"fmt"
	"io"

	"mediarepair/internal/parse"
)

// cmdVerify 校验输入 MP4 结构，成功后输出摘要；失败时定位错误。
func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify")
	input := fs.String("input", "", "输入 MP4 路径（必填）")
	timeline := fs.String("timeline", "", "时间线 sidecar 路径（可选）")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return 2
	}
	if *input == "" {
		fmt.Fprintln(stderr, "参数错误: --input 必填")
		return 2
	}
	mf, err := parse.OpenMP4(*input)
	if err != nil {
		var recoverable bool
		if fe, ok := asFault(err); ok {
			recoverable = fe.Recoverable
			fmt.Fprintf(stdout, "verified=false error_stage=%s source_offset=%d recoverable=%v\n",
				fe.Stage, fe.Offset, recoverable)
		} else {
			fmt.Fprintln(stdout, "verified=false error_stage=unknown")
		}
		reportError(stderr, err)
		return exitCodeOf(err)
	}
	defer mf.Close()
	if err := mf.ValidateSamples(); err != nil {
		if fe, ok := asFault(err); ok {
			fmt.Fprintf(stdout, "verified=false error_stage=%s source_offset=%d recoverable=%v\n",
				fe.Stage, fe.Offset, fe.Recoverable)
		}
		reportError(stderr, err)
		return exitCodeOf(err)
	}
	tlPath := *timeline
	if tlPath == "" {
		tlPath = defaultTimelinePath(*input)
	}
	if _, err := parse.LoadTimeline(tlPath); err != nil {
		if fe, ok := asFault(err); ok {
			fmt.Fprintf(stdout, "verified=false error_stage=%s source_offset=%d recoverable=%v\n",
				fe.Stage, fe.Offset, fe.Recoverable)
		}
		reportError(stderr, err)
		return exitCodeOf(err)
	}
	fmt.Fprintf(stdout, "verified=true tracks=%d samples=%d\n", len(mf.Tracks()), mf.TotalSamples())
	for _, t := range mf.Tracks() {
		fmt.Fprintf(stdout, "track=%d kind=%s timescale=%d samples=%d\n", t.ID, t.Kind, t.Timescale, mf.TrackSampleCount(t.ID))
	}
	return 0
}
