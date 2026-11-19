package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"mediasync/internal/audio"
	"mediasync/internal/fingerprint"
	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/segment"
	"mediasync/internal/subtitle"
	"mediasync/internal/testdata"
	"mediasync/internal/timeline"
)

// Options drive one pipeline run. Everything is derived from Seed so that the
// same options always produce the same report.
type Options struct {
	Seed          int64
	PacketLoss    int
	BandwidthKbps []int
	Frames        string
	TargetMS      int64
	OverlapMS     int64
	Window        int
	PCMBytes      []byte
	Clock         timeline.Clock
	OutputPath    string
	// Injected faults expected from the fixture generator. Issues matching
	// these are marked Expected in the report and do not change exit code.
	Expected map[media.Code]bool
}

// DefaultOptions mirror the demo invocation.
func DefaultOptions() Options {
	return Options{
		Seed:          42,
		PacketLoss:    4,
		BandwidthKbps: []int{800, 2400, 600},
		Frames:        "25",
		TargetMS:      1500,
		OverlapMS:     200,
		Window:        64,
		Expected:      map[media.Code]bool{},
	}
}

type stageTimer struct {
	name  string
	start time.Time
}

func (r *Report) begin(name string) stageTimer { return stageTimer{name: name, start: time.Now()} }

func (r *Report) finish(t stageTimer, ok bool, detail string, issues int) {
	r.Stages = append(r.Stages, StageReport{
		Name: t.name, OK: ok, Detail: detail, Issues: issues,
		ElapsedUS: time.Since(t.start).Microseconds(),
	})
}

// collect appends issues that were not deliberately injected. Any such
// recoverable issue moves the run to exit code 2.
func (r *Report) collect(opts Options, issues ...*media.Issue) {
	r.collectMarked(opts, false, issues...)
}

// collectExpected appends issues that the fixture generator injected on
// purpose: they are reported in full but do not change the exit code.
func (r *Report) collectExpected(opts Options, issues ...*media.Issue) {
	r.collectMarked(opts, true, issues...)
}

func (r *Report) collectMarked(opts Options, injected bool, issues ...*media.Issue) {
	for _, is := range issues {
		if is == nil {
			continue
		}
		expected := injected || opts.Expected[is.Code]
		r.Issues = append(r.Issues, IssueReport{
			Stage: string(is.Stage), Input: string(is.Input), Code: string(is.Code),
			File: is.File, Record: is.Record, Message: is.Message,
			Offset: is.Offset, HasOffset: is.HasOffset,
			PacketSeq: is.PacketSeq, HasSeq: is.HasSeq,
			Fatal: is.Fatal, Expected: expected,
		})
		if is.HasOffset {
			r.ErrorsWithOffset++
		}
		r.TotalIssues++
		if is.Recoverable() {
			r.RecoverableIssues++
		}
		if expected {
			r.ExpectedIssues++
		}
	}
}

func dedupeIssues(issues []*media.Issue) []*media.Issue {
	seen := map[string]struct{}{}
	out := issues[:0]
	for _, is := range issues {
		key := fmt.Sprintf("%s|%s|%d|%d|%s", is.Stage, is.File, is.Offset, is.Record, is.Code)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, is)
	}
	return out
}

func strList(reasons []segment.DegradeReason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, string(r))
	}
	return out
}

func silenceLabels(runs []audio.Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.String())
	}
	return out
}

// camera bundles everything a single camera contributes to the run.
type camera struct {
	name     string
	subs     *subtitle.Result
	tracks   []*mp4.Track
	file     []byte
	pcm      []int16
	pcmFmt   audio.Format
	silence  *audio.Result
	timebase timeline.Timebase
	segments []segment.Segment
	clips    []fingerprint.Clip
}

func (c *camera) video() *mp4.Track {
	for _, t := range c.tracks {
		if t.Handler == "vide" && len(t.Samples) > 0 {
			return t
		}
	}
	for _, t := range c.tracks {
		if len(t.Samples) > 0 {
			return t
		}
	}
	return nil
}

// transcriptAt returns the plain text overlapping a segment window.
func (c *camera) transcriptAt(startNS, endNS int64) string {
	if c.subs == nil {
		return ""
	}
	var parts []string
	for _, cue := range c.subs.Cues {
		if cue.EndNS > startNS && cue.StartNS < endNS {
			parts = append(parts, cue.Content())
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// silenceBetween converts absolute silence runs to nanosecond offsets relative
// to the clip start.
func (c *camera) silenceBetween(startNS, endNS int64) []int64 {
	if c.silence == nil {
		return nil
	}
	var out []int64
	for _, run := range c.silence.Runs {
		if run.EndNS <= startNS || run.StartNS >= endNS {
			continue
		}
		out = append(out, run.StartNS-startNS, run.EndNS-startNS)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func fixtureSubs(seed int64) *subtitle.Result {
	srt := testdata.GenerateSRT(testdata.SubtitleClean, seed)
	return subtitle.ParseSRT(srt, "cam-a.srt")
}

func fixtureSubsVTT(seed int64) *subtitle.Result {
	vtt := testdata.GenerateSRT(testdata.SubtitleWebVTT, seed)
	return subtitle.ParseWebVTT(vtt, "cam-b.vtt")
}
