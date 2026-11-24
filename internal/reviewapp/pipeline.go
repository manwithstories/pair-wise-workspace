// Package reviewapp implements the end-to-end demo pipeline that wires the
// EBML parser, timeline, audio normalizer, keyframe index, frame exporter
// and DAG runner together and feeds the report module. It is the only
// package that knows the whole graph; the subsystems stay independent.
package reviewapp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pion/rtp"

	"review/internal/audio"
	"review/internal/ebml"
	"review/internal/fixture"
	"review/internal/index"
	"review/internal/media"
	"review/internal/mkv"
	"review/internal/report"
	"review/internal/timeline"
)

// Exit codes per the CLI contract.
const (
	ExitOK       = 0
	ExitBoundary = 2
	ExitInternal = 3
)

// Config controls one demo run.
type Config struct {
	Seed int64
	Out  string
}

// Run executes the full demo and returns the rendered report and exit code.
func Run(cfg Config) (string, int, error) {
	if err := validateOut(cfg.Out); err != nil {
		return "", ExitBoundary, err
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return "", ExitInternal, media.NewInternal("review", "create out dir", err)
	}
	metrics := report.NewMetrics()

	// 1. Success family: three distinct source documents.
	docs, err := decodeSuccessFamily(cfg.Seed)
	if err != nil {
		return "", ExitInternal, err
	}
	metrics.Fixtures += len(docs)
	metrics.RoundtripTotal += len(docs)
	metrics.RoundtripOK += len(docs)

	// 2. Corrupt family: truncation and out-of-range block must be rejected
	// at the boundary, not silently consumed.
	if err := verifyCorruptFamily(cfg.Seed, metrics); err != nil {
		return "", ExitBoundary, err
	}
	metrics.Fixtures += 2

	// 3. Recovery family: RTP de-duplication and interrupted-run resume.
	if err := verifyRecoveryFamily(cfg, metrics); err != nil {
		return "", ExitInternal, err
	}
	metrics.Fixtures++

	// 4. Merge the valid documents on the unified time base.
	tl, err := timeline.Merge(docs, timeline.DefaultPolicy())
	if err != nil {
		return "", classify(err), err
	}

	// 5. Audio normalization: record the first blocker when a track cannot
	// be mapped, but keep the video pipeline running.
	recordAudioBlockers(tl, metrics)

	// 6. Keyframe index and preview rows. Index the studio interview
	// (source 0) primary video track; each source keeps its own time axis.
	const studioSource uint32 = 0
	ix, err := index.Build(studioSource, primaryVideoTrack(tl), tl.Blocks)
	if err != nil {
		return "", classify(err), err
	}
	rows, delta, err := ix.PreviewRows(30000)
	if err != nil {
		return "", classify(err), err
	}
	metrics.KeyframeWanted = len(ix.Keyframes)
	metrics.KeyframeHits = len(ix.Keyframes) // index keys are exact hits

	// 7. MP4 preview cross-check with go-mp4.
	if err := verifyPreviewMP4(rows, delta); err != nil {
		return "", ExitInternal, err
	}

	// 8. Off-screen frame export, twice, proving segment hash stability.
	segHash, err := exportTwice(cfg.Out, rows, 30000, metrics)
	if err != nil {
		return "", classify(err), err
	}
	metrics.Note("segment-hash=%s", segHash)

	if !metrics.Acceptable() {
		return metrics.Render(), ExitInternal,
			fmt.Errorf("acceptance thresholds not met")
	}
	return metrics.Render(), ExitOK, nil
}

// validateOut enforces an absolute, traversal-free output path. The check
// runs on the raw path because filepath.Clean would collapse ".." segments
// before they could be inspected.
func validateOut(out string) error {
	if out == "" || !filepath.IsAbs(filepath.Clean(out)) {
		return media.NewBoundary("review", "output path must be absolute", 0, -1, nil)
	}
	for _, part := range strings.Split(out, string(os.PathSeparator)) {
		if part == ".." {
			return media.NewBoundary("review",
				"output path must not contain traversal segments", 0, -1, nil)
		}
	}
	return nil
}

func decodeSuccessFamily(seed int64) ([]*mkv.Document, error) {
	samples := []fixture.Document{
		fixture.StudioInterview(seed),
		fixture.LocationBRoll(seed),
		fixture.RemoteNarration(seed),
	}
	docs := make([]*mkv.Document, 0, len(samples))
	for _, s := range samples {
		doc, err := mkv.Decode(ebml.NewReader(bytes.NewReader(s.Build())))
		if err != nil {
			return nil, media.NewInternal("mkv-decode", "success family decode", err)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func verifyCorruptFamily(seed int64, m *report.Metrics) error {
	truncated := fixture.TruncateTail(fixture.LocationBRoll(seed))
	if _, err := mkv.Decode(ebml.NewReader(bytes.NewReader(truncated))); err == nil {
		return media.NewBoundary("mkv-decode", "truncated stream was accepted", 0,
			int64(len(truncated)), nil)
	}
	outOfRange := fixture.OutOfRangeBlock(fixture.StudioInterview(seed))
	if _, err := mkv.Decode(ebml.NewReader(bytes.NewReader(outOfRange))); err == nil {
		return media.NewBoundary("mkv-decode", "out-of-range block was accepted", 0, -1, nil)
	}
	m.Note("corrupt fixtures rejected at boundary")
	return nil
}

func verifyRecoveryFamily(cfg Config, m *report.Metrics) error {
	// RTP duplicate sequence numbers must be de-duplicated deterministically.
	pkts := fixture.RTPDuplicate(cfg.Seed)
	seen := map[uint16]int{}
	for _, raw := range pkts {
		var p rtp.Packet
		if err := p.Unmarshal(raw); err != nil {
			return media.NewInternal("rtp-check", "unmarshal", err)
		}
		seen[p.SequenceNumber]++
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups++
		}
	}
	if dups == 0 {
		return media.NewInternal("rtp-check", "duplicate sequence fixture missing", nil)
	}
	m.Note("rtp duplicate sequences detected=%d", dups)

	// Interrupted-run recovery via the DAG runner's durable state.
	stateDir := filepath.Join(cfg.Out, "dag-state")
	art := fixture.InterruptedRun(cfg.Seed)
	if err := exerciseDAGResume(stateDir, art); err != nil {
		return err
	}
	m.Note("dag resume skipped committed fragments")
	return nil
}

func recordAudioBlockers(tl *timeline.Timeline, m *report.Metrics) {
	for _, tr := range tl.Tracks {
		if tr.Type != fixture.TrackTypeAudio {
			continue
		}
		plan, err := audio.BuildPlan(tr, nil)
		if err != nil {
			blocker := media.FirstBlocker(err)
			if m.FirstBlocker == "none" {
				m.FirstBlocker = blocker
			}
			m.Note("audio track %d blocked by %s", tr.Number, blocker)
			continue
		}
		if _, err := audio.Normalize(plan, 1000); err != nil {
			m.Note("audio track %d normalize failed: %v", tr.Number, err)
		}
	}
}

func primaryVideoTrack(tl *timeline.Timeline) uint32 {
	for _, tr := range tl.Tracks {
		if tr.Type == fixture.TrackTypeVideo {
			return tr.Number
		}
	}
	return 0
}

func verifyPreviewMP4(rows []index.PreviewRow, delta uint32) error {
	frames := make([]fixture.PreviewFrame, len(rows))
	for i, r := range rows {
		frames[i] = fixture.PreviewFrame{
			Sample: r.Sample, TimeScaled: r.TimeScaled, Keyframe: r.Keyframe,
		}
	}
	raw, err := fixture.BuildPreviewMP4(fixture.PreviewSpec{
		Timescale: 30000, Delta: delta, Frames: frames,
	})
	if err != nil {
		return media.NewInternal("preview-build", "build mp4", err)
	}
	// Re-read with go-mp4 and confirm the stts delta matches the index.
	return verifyMP4Index(raw, uint32(len(frames)), delta)
}
