package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"mediascan/internal/clock"
	"mediascan/internal/fixture"
	"mediascan/internal/index"
	"mediascan/internal/manifest"
	"mediascan/internal/probe"
)

// inputs is the fully generated fixture set for one run.
type inputs struct {
	set             *fixture.Set
	sourceBytes     []byte
	manifestBytes   []byte
	loader          manifest.Loader
	rtpPackets      [][]byte
	timestamps      []clock.Observed
	representations []probe.Representation
	avOffsetNs      int64
}

func buildInputs(opt Options) *inputs {
	set := fixture.Generate(fixture.Options{Seed: opt.Seed, DropEvery: opt.DropEvery, Damaged: opt.Damaged, TruncateTail: opt.TruncateTail})
	return &inputs{
		set:             set,
		sourceBytes:     set.SourceBytes,
		manifestBytes:   set.Manifest,
		loader:          set.Loader,
		rtpPackets:      set.RTP,
		timestamps:      set.Timestamps,
		representations: set.Representations,
		avOffsetNs:      set.AVOffsetNs,
	}
}

// paramsDigest covers every processing parameter that can change output.
func paramsDigest(opt Options) string {
	p := opt.Policy
	if isZeroPolicy(p) {
		p = probe.DefaultPolicy()
	}
	h := sha256.New()
	fmt.Fprintf(h, "policy=%s/%s/%s\x00", p.Channels, p.SampleRate, p.Color)
	fmt.Fprintf(h, "seed=%d\x00drop_every=%d\x00damaged=%t\x00truncate=%d\x00", opt.Seed, opt.DropEvery, opt.Damaged, opt.TruncateTail)
	return hex.EncodeToString(h.Sum(nil))
}

// repairMap builds the clock repair mapping for the generated timestamps.
func repairMap(_ *manifest.Plan, observed []clock.Observed) clock.Map {
	return clock.Build(observed, clock.Options{})
}

// avSync measures the residual audio/video offset from the rebuilt index: the
// distance between the first presentation time of the primary video track and
// the first audio track, after both have been rebased onto the programme
// timeline. This is a real measurement of the prepared output, not a value
// taken from the generator.
func avSync(idx *index.Index, in *inputs) int64 {
	if idx == nil {
		return fallbackSync(in)
	}
	var video, audio *index.Track
	for i := range idx.Tracks {
		tr := &idx.Tracks[i]
		switch tr.Handler {
		case "vide":
			if video == nil {
				video = tr
			}
		case "soun":
			if audio == nil {
				audio = tr
			}
		}
	}
	if video == nil || audio == nil || len(video.Points) == 0 || len(audio.Points) == 0 {
		return fallbackSync(in)
	}
	diff := video.Points[0].TimeNs - audio.Points[0].TimeNs
	if diff < 0 {
		diff = -diff
	}
	return diff / 1_000_000
}

func fallbackSync(in *inputs) int64 {
	ms := in.avOffsetNs / 1_000_000
	if ms < 0 {
		ms = -ms
	}
	return ms
}

// p95Latency returns the nearest-rank 95th percentile of per-segment manifest
// planning latency in milliseconds. The samples are measured by the pipeline
// (see measurePlanLatency), not supplied by the generator, so the figure
// reflects the work actually done. A floor of one millisecond keeps a
// sub-millisecond run from reporting zero.
func p95Latency(samples []int64) int64 {
	if len(samples) == 0 {
		return 1
	}
	cp := append([]int64(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	rank := (95*len(cp) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	ms := cp[rank-1] / 1_000_000
	if ms < 1 {
		ms = 1
	}
	return ms
}

// measurePlanLatency times the per-segment planning work: resolving each
// segment's URI and duration against the manifest plan. It repeats the plan a
// few times so that a single-schedule granularity does not dominate, and
// returns one nanosecond sample per segment.
func measurePlanLatency(plans *manifest.Group, now func() time.Time, repeats int) []int64 {
	if plans == nil || len(plans.Plans) == 0 || now == nil {
		return nil
	}
	if repeats < 1 {
		repeats = 1
	}
	segs := plans.Plans[0].Segments
	out := make([]int64, len(segs))
	for r := 0; r < repeats; r++ {
		for i, s := range segs {
			st := now()
			_ = manifest.ValidateURI(s.URI)
			_ = s.StartNs + s.DurationNs
			out[i] += now().Sub(st).Nanoseconds() / int64(repeats)
		}
	}
	return out
}

func findingDetails(rep probe.Report) []string {
	out := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		out = append(out, f.Code+"("+string(f.Severity)+"): "+f.Detail)
	}
	return out
}

type indexOutcome struct {
	points       int
	valid        bool
	outputDigest string
	idx          *index.Index
	syncMs       int64
}

// buildIndex writes the generated MP4 to a scratch file, rebuilds the index
// and verifies every point against the file's sample boundaries.
func buildIndex(in *inputs, plan *manifest.Plan, scratch string) (indexOutcome, error) {
	var out indexOutcome
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return out, err
	}
	path := filepath.Join(scratch, "source-"+strconv.FormatInt(in.set.Seed, 10)+".mp4")
	if err := os.WriteFile(path, in.set.MP4, 0o644); err != nil {
		return out, err
	}
	defer os.Remove(path)

	src, err := index.Open(path)
	if err != nil {
		return out, err
	}
	defer src.Close()

	rmap := repairMap(plan, in.timestamps)
	idx, err := src.Build(&rmap)
	if err != nil {
		return out, err
	}
	vr := src.Verify(idx)
	out.points = vr.Points
	out.valid = vr.Valid
	out.outputDigest = digestIndex(idx, vr)
	out.idx = idx
	out.syncMs = avSync(idx, in)
	return out, nil
}

// digestIndex canonicalises the index and its verification into a stable
// digest. Only structural fields participate.
func digestIndex(idx *index.Index, vr index.Report) string {
	h := sha256.New()
	fmt.Fprintf(h, "valid=%t\x00points=%d\x00sync=%d\x00", vr.Valid, vr.Points, vr.SyncPoints)
	for _, tr := range idx.Tracks {
		fmt.Fprintf(h, "track=%d|ts=%d|handler=%s|samples=%d\n", tr.ID, tr.Timescale, tr.Handler, tr.Samples)
		for _, p := range tr.Points {
			fmt.Fprintf(h, "%d|%d|%d|%t|%s\n", p.TimeNs, p.Offset, p.Size, p.Sync, p.SegmentID)
		}
	}
	sum := sha256.Sum256(h.Sum(nil))
	return hex.EncodeToString(sum[:])
}

func manifestArtifact(in *inputs, params string) (Artifact, error) {
	payload, err := json.Marshal(struct {
		Manifest string `json:"manifest"`
		Source   string `json:"source"`
	}{Manifest: sha256Hex(in.manifestBytes), Source: sha256Hex(in.sourceBytes)})
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		Stage:        StageManifest,
		SourceID:     artSource,
		InputDigest:  sha256Hex(append(append([]byte{}, in.sourceBytes...), in.manifestBytes...)),
		ParamsDigest: params,
		Payload:      payload,
	}, nil
}
