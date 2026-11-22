// Package pipeline wires the parsers, timecode clock, stream analyzers,
// checkpoint store and verification rules into one deterministic scan. Work
// is organized as segments (one input file per segment); a checkpoint is
// written atomically only after a segment is fully verified, so resume
// cursors always rest on verified boundaries.
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"mediascan/internal/checkpoint"
	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

// InputKind identifies how a source file is parsed.
type InputKind string

const (
	KindSRT InputKind = "srt"
	KindVTT InputKind = "vtt"
	KindMP4 InputKind = "mp4"
	KindRTP InputKind = "rtp"
)

// Input is one scan source.
type Input struct {
	Path string
	Kind InputKind
}

// Params are the run parameters folded into the checkpoint fingerprint.
type Params struct {
	Rate clock.Rate
}

func (p Params) String() string {
	return fmt.Sprintf("rate=%d/%d/df=%v", p.Rate.Num, p.Rate.Den, p.Rate.DropFrame)
}

// SegmentReport is the verified outcome for one input file.
type SegmentReport struct {
	Index        int       `json:"index"`
	Source       string    `json:"source"`
	Kind         InputKind `json:"kind"`
	Bytes        int64     `json:"bytes"`
	EventCount   int       `json:"event_count"`
	BoxCount     int       `json:"box_count"`
	PacketCount  int       `json:"packet_count"`
	KeyframeGaps int       `json:"keyframe_gaps"`
	RTPGaps      int       `json:"rtp_gaps"`
	DamageOffset int64     `json:"damage_offset"`
	Recovered    bool      `json:"recovered"`
	Start        int64     `json:"verified_start"`
	End          int64     `json:"verified_end"`
}

// ErrInterrupted marks a clean stop requested before all segments ran.
var ErrInterrupted = errors.New("scan interrupted after verified segment boundary")

// Result is the full scan output consumed by the report package.
type Result struct {
	Params            Params
	Segments          []SegmentReport
	Faults            []fault.Fault
	Fingerprints      map[string]string
	Interrupted       bool
	RecoveredSegments int
	KeyframeGapTotal  int
}

// Run executes (or resumes) a scan over inputs, persisting progress in dir.
// interruptAfter > 0 stops the run cleanly once that many newly verified
// segments have been processed in this invocation.
func Run(dir string, inputs []Input, params Params, interruptAfter int) (*Result, error) {
	if int64(len(inputs)) > fault.MaxInputSize {
		return nil, errors.New("input count absurdly large")
	}
	store, err := checkpoint.Load(dir)
	if err != nil {
		return nil, err // corrupt checkpoint: exit code 2
	}
	fps := fingerprints(inputs)
	if store == nil {
		store = &checkpoint.Store{Fingerprints: fps, Params: params.String()}
	} else {
		// Verify against the ORIGINAL stored fingerprints before refreshing
		// the map; otherwise a changed input would overwrite the very hash
		// that is supposed to detect the tampering.
		if err := store.VerifyParams(params.String()); err != nil {
			return nil, err
		}
		if err := store.VerifyFingerprint(); err != nil {
			return nil, err
		}
		store.Fingerprints = fps
	}

	res := &Result{Params: params, Fingerprints: fps}

	// Resume: reconstruct the already-verified portion from the checkpoint
	// so the rendered report covers every segment, not just newly scanned
	// ones. Only verified segment boundaries are ever persisted, so the
	// restored data is complete by construction.
	startIndex := store.CursorIndex
	if startIndex > len(inputs) {
		return nil, errors.New("checkpoint cursor exceeds current input list")
	}
	for i := 0; i < startIndex; i++ {
		if i >= len(store.SegmentDetails) {
			return nil, errors.New("checkpoint missing persisted segment detail")
		}
		var seg SegmentReport
		if err := json.Unmarshal(store.SegmentDetails[i], &seg); err != nil {
			return nil, fmt.Errorf("decode verified segment %d: %w", i, err)
		}
		res.Segments = append(res.Segments, seg)
	}
	res.Faults = append(res.Faults, store.Faults...)
	res.RecoveredSegments = store.Recovered
	res.KeyframeGapTotal = store.KeyframeGaps

	processed := 0
	for i := startIndex; i < len(inputs); i++ {
		seg, flts, err := scanOne(i, inputs[i], params)
		if err != nil {
			return nil, err
		}
		cpSeg := checkpoint.Segment{
			ID: fmt.Sprintf("seg-%d", i), Source: inputs[i].Path,
			Start: seg.Start, End: seg.End, Stage: checkpoint.StageVerify,
		}
		// Persist the complete verified segment before touching the in-memory
		// result, so the cursor never runs ahead of durable data.
		if err := store.RecordVerified(dir, cpSeg, seg, flts, seg.Recovered, seg.KeyframeGaps); err != nil {
			return nil, err
		}
		res.Faults = append(res.Faults, flts...)
		if seg.Recovered {
			res.RecoveredSegments++
		}
		res.KeyframeGapTotal += seg.KeyframeGaps
		res.Segments = append(res.Segments, seg)
		processed++
		if interruptAfter > 0 && processed >= interruptAfter {
			res.Interrupted = true
			res.sortFaults()
			return res, ErrInterrupted
		}
	}
	store.CursorStage = checkpoint.StageDone
	if err := store.Save(dir); err != nil {
		return nil, err
	}
	res.sortFaults()
	return res, nil
}

func (r *Result) sortFaults() {
	sort.SliceStable(r.Faults, func(i, j int) bool {
		a, b := r.Faults[i], r.Faults[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		return a.Message < b.Message
	})
}

// fingerprints hashes every input up front; failure aborts before any work.
func fingerprints(inputs []Input) map[string]string {
	out := make(map[string]string, len(inputs))
	for _, in := range inputs {
		fp, err := checkpoint.FingerprintFile(in.Path)
		if err != nil {
			out[in.Path] = "error:" + err.Error()
			continue
		}
		out[in.Path] = fp
	}
	return out
}

// scanOne parses and verifies a single input, isolating its damage.
func scanOne(index int, in Input, params Params) (SegmentReport, []fault.Fault, error) {
	info, err := os.Stat(in.Path)
	if err != nil {
		return SegmentReport{}, nil, err
	}
	if info.Size() > fault.MaxInputSize {
		return SegmentReport{}, nil, fmt.Errorf("%s exceeds 2 GiB limit", in.Path)
	}
	seg := SegmentReport{
		Index: index, Source: in.Path, Kind: in.Kind, Bytes: info.Size(),
		DamageOffset: -1, Start: 0, End: info.Size(),
	}
	var flts []fault.Fault

	switch in.Kind {
	case KindSRT, KindVTT:
		flts, seg.EventCount, err = scanSubtitle(in, params)
	case KindMP4:
		flts, seg, err = scanMP4(in, seg)
	case KindRTP:
		flts, seg, err = scanRTP(in, seg)
	default:
		return seg, nil, fmt.Errorf("unknown input kind %q", in.Kind)
	}
	if err != nil {
		return seg, nil, err
	}
	// A segment that carried faults but still produced usable results is
	// classified as recovered, not failed.
	seg.Recovered = len(flts) > 0
	return seg, flts, nil
}
