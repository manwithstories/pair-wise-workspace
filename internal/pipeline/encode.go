package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fashionweek/mediad/internal/align"
	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/checkpoint"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/encode"
	"github.com/fashionweek/mediad/internal/hls"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/provenance"
)

type encodeDeps struct {
	job        *JobConfig
	opts       Options
	runner     *encode.Runner
	timeline   *align.Timeline
	cuts       []align.Cut
	cas        *cache.Store
	builder    *provenance.Builder
	results    []*CameraResult
	segDur     clock.Rat
	configHash string
}

// segmentIdentity is the global unit of checkpointing: one variant of one cut
// of one camera. The synthetic interrupt counts these units globally so that
// "after segment 3" is deterministic regardless of camera ordering.
type segmentIdentity struct {
	camera  string
	variant string
	seq     int
}

// encodeAll drives every (camera, variant, cut) unit through cache lookup,
// checkpoint resume and on-demand encoding, enforcing the synthetic interrupt.
func encodeAll(ctx context.Context, d encodeDeps) (done, total int, interrupted bool, err error) {
	ladder := ladderFor(d.job)
	cp, err := checkpoint.Open(filepath.Join(d.opts.WorkDir, "checkpoint"), d.job.JobID)
	if err != nil {
		return 0, 0, false, err
	}

	// Enumerate the full deterministic unit order.
	units := make([]segmentIdentity, 0)
	for _, cut := range d.cuts {
		for _, v := range ladder {
			for _, res := range d.results {
				units = append(units, segmentIdentity{
					camera: res.Name, variant: v.Name, seq: cut.Index,
				})
			}
		}
	}
	total = len(units)

	// Pre-register camera provenance (input digest + shared time span).
	spanByCamera := map[string]*provenance.SourceSpan{}
	for _, res := range d.results {
		c := cameraFromResults(d.timeline, res.Name)
		span := spanFor(d.timeline, c)
		spanByCamera[res.Name] = span
		d.builder.RecordCamera(res.Name, res.Path, res.InputSHA, true, span)
	}

	for _, u := range units {
		if err := ctx.Err(); err != nil {
			return cp.CompletedCount(), total, false,
				media.New(media.PhaseEncode, media.KindTool, "", -1, "context cancelled").Wrap(err)
		}
		res := findResult(d.results, u.camera)
		variant := findVariant(ladder, u.variant)
		cut := d.cuts[u.seq]

		dest := segmentPath(d.opts.WorkDir, u.camera, u.variant, u.seq)
		initDest := initPath(d.opts.WorkDir, u.camera, u.variant)

		// Resume path: a durably recorded segment is reused only after its
		// on-disk hash is re-verified against the checkpoint digest.
		if state, ok := cp.Segment(segmentCheckpointKey(u)); ok {
			if reuseIfIntact(cp, state, variant, dest, initDest, d) {
				// The provenance builder is fresh per process, so a reused
				// segment must still be recorded or the receipt would omit it.
				if err := d.builder.AddSegment(u.camera, u.variant, u.seq, dest); err != nil {
					return done, total, false, err
				}
				done++
				continue
			}
		} else if orphanExists(dest) {
			// No checkpoint but a file survived a crash: it is untrusted and
			// must be rebuilt rather than silently adopted.
			if err := os.Remove(dest); err != nil {
				return done, total, false, media.New(media.PhaseCheckpoint,
					media.KindTool, dest, -1, "cannot remove untrusted orphan").Wrap(err)
			}
		}

		if err := encodeUnit(ctx, d, res, variant, cut, u, dest, initDest); err != nil {
			return done, total, false, err
		}
		if err := recordUnit(cp, d, u, dest, initDest, cut); err != nil {
			return done, total, false, err
		}
		done++

		// Synthetic interrupt: stop after N completed units with code 75.
		if d.opts.InterruptAfter > 0 && done >= d.opts.InterruptAfter {
			return done, total, true, nil
		}
	}
	return done, total, false, nil
}

// reuseIfIntact validates a checkpointed segment and its init against the live
// files and returns true only when both digests still match.
func reuseIfIntact(cp *checkpoint.Store, state checkpoint.SegmentState,
	variant hls.Variant, dest, initDest string, d encodeDeps) bool {
	want, ok := state.Outputs[variant.Name]
	if !ok {
		return false
	}
	if !cp.AdoptOrphan(dest, want) {
		return false
	}
	// The init section must also be intact; otherwise the variant is unusable.
	if _, err := os.Stat(initDest); err != nil {
		return false
	}
	return true
}

func encodeUnit(ctx context.Context, d encodeDeps, res *CameraResult,
	variant hls.Variant, cut align.Cut, u segmentIdentity, dest, initDest string) error {
	vBit, aBit := bitrateFor(variant)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	// Cache lookup: the key binds the source digest, the segment identity
	// (variant + cut), the configuration and the FFmpeg version. Including the
	// segment identity keeps every (variant, cut) in its own entry instead of
	// letting same-camera segments overwrite one another. A hit is trusted only
	// after re-verifying the input source and the cached output digest.
	key := segmentCacheKey(res.InputSHA, d.configHash, d.runner.Version(), u)
	entry, err := d.cas.Get(key)
	if err != nil {
		return err
	}
	if entry != nil {
		name := casOutputName(u)
		if verr := d.cas.Verify(entry, res.InputSHA); verr == nil {
			if rerr := d.cas.Restore(entry, name, dest); rerr == nil {
				if ierr := ensureInit(initDest); ierr == nil {
					return nil
				}
				// Init missing: fall through to a full rebuild.
			}
		}
	}

	work := filepath.Join(d.opts.WorkDir, "scratch",
		fmt.Sprintf("%s-%s-%03d", u.camera, u.variant, u.seq))
	rng := cut.Ranges[u.camera]
	start := rng.KfAt.Seconds()
	dur, err := cut.Till.Sub(cut.At)
	if err != nil {
		return err
	}
	out, err := d.runner.EncodeSegment(ctx, encode.SegmentRequest{
		Input: res.Path, WorkDir: work,
		StartSec: start, DurationSec: dur.Seconds(),
		Width: variant.Width, Height: variant.Height,
		VideoBitrate: vBit, AudioBitrate: aBit,
	})
	if err != nil {
		return err
	}
	if err := moveAtomic(out.Segment, dest); err != nil {
		return err
	}
	// Publish the init section once; keep it byte-stable by only writing when
	// absent (the first segment's init is canonical for this variant).
	if _, err := os.Stat(initDest); os.IsNotExist(err) {
		if err := moveAtomic(out.Init, initDest); err != nil {
			return err
		}
	}
	// Store the freshly built segment in the CAS so a later identical job can
	// restore it without re-running FFmpeg.
	if _, err := d.cas.Put(key, res.InputSHA, d.runner.Version(),
		[]byte(d.configHash), map[string]string{casOutputName(u): dest}); err != nil {
		return err
	}
	return nil
}

// segmentCacheKey derives a per-segment content key. The source digest already
// separates cameras; folding in the variant and cut index separates the 15
// outputs within one camera, while config and tool version retain the required
// invalidation dimensions.
func segmentCacheKey(inputSHA, configHash, toolVersion string, u segmentIdentity) string {
	identity := fmt.Sprintf("%s|%s|seq=%d", u.camera, u.variant, u.seq)
	composite := cache.HashBytes([]byte(configHash + "|" + identity))
	return cache.Key(inputSHA, []byte(composite), toolVersion)
}

// casOutputName gives each (variant, cut) a stable logical name inside a
// camera-scoped cache key; the source digest already separates cameras.
func casOutputName(u segmentIdentity) string {
	return fmt.Sprintf("%s-seg-%03d", u.variant, u.seq)
}

// ensureInit reports whether the variant init section is already published.
// When absent the caller falls back to a full encode, which recreates it.
func ensureInit(initDest string) error {
	if _, err := os.Stat(initDest); err == nil {
		return nil
	}
	return media.New(media.PhaseCache, media.KindDigest, initDest, -1,
		"cache hit without a published init section; rebuild required")
}

func recordUnit(cp *checkpoint.Store, d encodeDeps, u segmentIdentity,
	dest, initDest string, cut align.Cut) error {
	hash, err := cache.HashFile(dest)
	if err != nil {
		return err
	}
	state := checkpoint.SegmentState{
		Index:    segmentCheckpointKey(u),
		Outputs:  map[string]string{u.variant: hash},
		CutAtNum: cut.At.Num, CutAtDen: cut.At.Den,
	}
	if err := cp.Record(state); err != nil {
		return err
	}
	return d.builder.AddSegment(u.camera, u.variant, u.seq, dest)
}

func segmentCheckpointKey(u segmentIdentity) int {
	// Pack the triple into a stable non-negative index for the checkpoint.
	return u.seq*1000 + variantRank(u.variant)*100 + cameraRank(u.camera)
}

func variantRank(name string) int {
	for i, v := range hls.DefaultLadder() {
		if v.Name == name {
			return i + 1
		}
	}
	return 9
}

func cameraRank(name string) int {
	// Camera names are short labels; derive a stable small rank from bytes.
	h := 0
	for _, c := range name {
		h = (h*31 + int(c)) % 90
	}
	return h + 1
}

func findResult(rs []*CameraResult, name string) *CameraResult {
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func findVariant(ladder []hls.Variant, name string) hls.Variant {
	for _, v := range ladder {
		if v.Name == name {
			return v
		}
	}
	return hls.Variant{}
}

func cameraFromResults(tl *align.Timeline, name string) *align.Camera {
	for _, c := range tl.Cameras {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func spanFor(tl *align.Timeline, c *align.Camera) *provenance.SourceSpan {
	if c == nil {
		return nil
	}
	frames := tl.Frames[c.Name]
	if len(frames) == 0 {
		return nil
	}
	last := frames[len(frames)-1]
	end, _ := last.At.Add(last.Dur)
	return provenance.SpanFromRat(c.Creation, cameraOffset(tl, c), tl.Start, end)
}

func cameraOffset(tl *align.Timeline, c *align.Camera) clock.Rat {
	frames := tl.Frames[c.Name]
	if len(frames) == 0 {
		z, _ := clock.New(0, 1)
		return z
	}
	off, _ := frames[0].At.Sub(frames[0].PTS)
	return off
}

func orphanExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func moveAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return media.New(media.PhaseEncode, media.KindTool, dst, -1,
			"cannot create destination dir").Wrap(err)
	}
	return cache.MoveAtomic(src, dst)
}
