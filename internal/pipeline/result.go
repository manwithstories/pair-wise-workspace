package pipeline

import (
	"fmt"
	"io"
	"strings"

	"pairflow/internal/media"
	"pairflow/internal/store"
	"pairflow/internal/transport"
)

// Result is the committed outcome of a full run.
type Result struct {
	RunDir   string
	Timeline string
	Frames   int
	Hashes   []string
	Audio    int64
	Report   transport.Report
}

// output returns the destination for stage progress lines. When the command
// adapter supplies a writer, lines stream immediately; otherwise they collect
// for the caller to flush.
func output(opts Options) io.Writer {
	if opts.Out != nil {
		return opts.Out
	}
	return defaultOut
}

var defaultOut = &safeBuilder{}

type safeBuilder struct{ strings.Builder }

// Drain hands the accumulated output to w and resets the buffer.
func Drain(w io.Writer) {
	if w == nil || defaultOut.Len() == 0 {
		return
	}
	_, _ = io.WriteString(w, defaultOut.String())
	defaultOut.Reset()
}

// manifestStages assembles the committed stage documents from the journal.
func manifestStages(wal *store.WAL, timeline string) []store.StageDoc {
	var out []store.StageDoc
	for _, r := range wal.Records() {
		if r.State != "done" || r.Timeline != timeline {
			continue
		}
		st := store.StageDoc{Name: r.Stage}
		for _, a := range r.Artifacts {
			st.Artifacts = append(st.Artifacts, a)
		}
		out = append(out, st)
	}
	return store.SortedStages(out)
}

// rollback logs the failure before removing the stage's uncommitted files.
// The returned error is always class-3 integrity so the process exit code
// reflects that a rollback happened.
func rollback(st *store.StageStager, cause *media.Error, wal *store.WAL, stage, fp string) *media.Error {
	_ = wal.Fail(stage, fp, cause.Error())
	if rerr := st.Rollback(); rerr != nil {
		e := media.Integrity("pipeline.rollback_failed",
			"stage %s failed (%s) and rollback also failed", stage, cause.Error())
		e.Cause = rerr
		return e.WithStage(stage)
	}
	e := media.Integrity("pipeline.rolled_back", "stage %s failed and was rolled back: %s", stage, cause.Error())
	e.Cause = cause
	return e.WithStage(stage)
}

// rolledBackStage reports the last stage that failed and rolled back.
func rolledBackStage(wal *store.WAL, timeline string) (string, bool) {
	for i := len(wal.Records()) - 1; i >= 0; i-- {
		r := wal.Records()[i]
		if r.State == "failed" && r.Timeline == timeline {
			return r.Stage, true
		}
	}
	return "", false
}

// Line renders one stage-progress line for tests and terminal output.
func Line(stage, state string, kv ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stage=%s state=%s", stage, state)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %s=%s", kv[i], kv[i+1])
	}
	return b.String()
}
