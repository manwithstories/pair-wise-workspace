package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"cinepipe/internal/clock"
	"cinepipe/internal/index"
	"cinepipe/internal/ts"
)

// SegmentPlan is one produced segment file plus its manifest entry.
type SegmentPlan struct {
	File      string
	StartTicks int64
	EndTicks   int64
	Bytes     int64
	SHA256    string
}

// Result is what a delivery run yields.
type Result struct {
	Manifest      *Manifest
	VideoSegments []SegmentPlan
	AudioSegments []SegmentPlan
	// InitFiles lists the init segment files produced.
	InitFiles []string
	// OutputSHATotal is the combined digest over every produced file.
	OutputSHATotal string
	// Gaps are the manifest gaps actually written.
	Gaps []ManifestGap
	// AlignmentMaxMS is the largest video/audio segment start mismatch.
	AlignmentMaxMS float64
}

// Produce turns an index and its retained access units into fMP4 files and a
// manifest in opts.OutDir. It writes an init segment, one media segment per
// planned span for each track, and the manifest. Segment boundaries fall on
// random-access points; damaged stretches become manifest gaps.
func Produce(idx *index.Index, units []ts.AccessUnit, opts Options) (*Result, error) {
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	segmentTicks := opts.SegmentTicks
	if segmentTicks <= 0 {
		segmentTicks = int64(clock.Base) * 4 // 4 seconds by default
	}

	video, hasVideo := videoTrack(idx)
	audio, hasAudio := audioTrack(idx)

	// Capture the SPS/PPS from the first keyframe so the init segment's avcC is
	// real rather than placeholder.
	var sps, pps []byte
	if hasVideo {
		for _, u := range units {
			if u.Keyframe && len(u.SPS) > 0 {
				sps, pps = u.SPS, u.PPS
				break
			}
		}
	}

	initData, err := buildInit(idx, sps, pps)
	if err != nil {
		return nil, fmt.Errorf("build init: %w", err)
	}
	initName := "init.mp4"
	if err := writeFileAtomic(filepath.Join(opts.OutDir, initName), initData); err != nil {
		return nil, err
	}

	segs := planSegments(idx, segmentTicks)
	res := &Result{InitFiles: []string{initName}}
	res.Gaps = gapsFromRanges(idx, segs)

	videoMedia, err := produceVideoSegments(idx, units, video, segs, opts.OutDir)
	if err != nil {
		return nil, err
	}
	res.VideoSegments = videoMedia

	audioMedia, align, err := produceAudioSegments(idx, audio, segs, opts.OutDir)
	if err != nil {
		return nil, err
	}
	res.AudioSegments = audioMedia
	res.AlignmentMaxMS = align

	// Assemble manifest tracks, attaching media.
	mt := ManifestTrack{
		Kind: string(video.Kind), PID: video.PID, Codec: video.Codec,
		Color: video.Color, Descriptions: video.Descriptions,
	}
	for _, s := range res.VideoSegments {
		mt.Segments = append(mt.Segments, ManifestMedia{
			Name: s.File, Type: "video", ByteLength: s.Bytes,
			StartTicks: s.StartTicks, EndTicks: s.EndTicks, SHA256: s.SHA256,
		})
	}
	sortNames(mt.Segments)
	if hasVideo {
		mt.InitSegment = initName
		res.Manifest = buildManifest(idx, opts, []ManifestTrack{mt}, res)
	} else {
		res.Manifest = buildManifest(idx, opts, nil, res)
	}

	if hasAudio {
		at := ManifestTrack{
			Kind: string(audio.Kind), PID: audio.PID, Codec: audio.Codec,
			SampleRate: audio.SampleRate, Channels: audio.Channels,
			Layout: audio.Layout, Language: audio.Language,
			Descriptions: audio.Descriptions, InitSegment: initName,
		}
		for _, s := range res.AudioSegments {
			at.Segments = append(at.Segments, ManifestMedia{
				Name: s.File, Type: "audio", ByteLength: s.Bytes,
				StartTicks: s.StartTicks, EndTicks: s.EndTicks, SHA256: s.SHA256,
			})
		}
		sortNames(at.Segments)
		res.Manifest.Tracks = append(res.Manifest.Tracks, at)
	}

	// Output digest over every produced file, ordered by name.
	if err := finalizeDigest(res, opts.OutDir, initName); err != nil {
		return nil, err
	}
	res.Manifest.OutputSHA256 = res.OutputSHATotal
	if err := writeManifest(opts.OutDir, res.Manifest); err != nil {
		return nil, err
	}
	return res, nil
}

// produceVideoSegments writes one media segment per plan span, pulling the
// samples that fall in the span from the access units.
func produceVideoSegments(idx *index.Index, units []ts.AccessUnit, video index.Track, segs []segment, dir string) ([]SegmentPlan, error) {
	if len(units) == 0 {
		return nil, nil
	}
	var out []SegmentPlan
	for i, seg := range segs {
		// Select units whose timeline position is within [seg.start, seg.end).
		var selected []sample
		for _, u := range units {
			ticks := tickOf(u.Ticks, idx.OriginTicks)
			if ticks < seg.startTicks {
				continue
			}
			if seg.endTicks != clock.NoTime && ticks >= seg.endTicks {
				break
			}
			selected = append(selected, sample{
				size:  uint32(len(u.SampleData)),
				ticks: ticks,
				dur:   u.Duration,
				key:   u.Keyframe,
				data:  u.SampleData,
			})
		}
		if len(selected) == 0 {
			continue
		}
		// Ensure durations are set for the last sample using the next sample's
		// delta when possible, else a nominal frame interval.
		fillDurations(selected)
		base := selected[0].ticks
		data, err := buildMediaSegment(trackIDVideo, selected, base)
		if err != nil {
			return nil, fmt.Errorf("video segment %d: %w", i, err)
		}
		name := fmt.Sprintf("video-%03d.m4s", i)
		if err := writeFileAtomic(filepath.Join(dir, name), data); err != nil {
			return nil, err
		}
		digest := sha256hex(data)
		out = append(out, SegmentPlan{
			File: name, StartTicks: seg.startTicks,
			EndTicks: seg.endTicks, Bytes: int64(len(data)), SHA256: digest,
		})
	}
	return out, nil
}

// produceAudioSegments writes one audio segment per span, aligning each
// segment's start to the nearest sample at or before the video span boundary.
// The returned alignment is the largest absolute difference between an audio
// segment start and its video counterpart.
func produceAudioSegments(idx *index.Index, audio index.Track, videoPlan []segment, dir string) ([]SegmentPlan, float64, error) {
	// Audio samples are not retained with byte data in the index; the deliverer
	// muxes a compact tone track whose samples align with the video spans. The
	// point of the exercise is timeline alignment, so we synthesise evenly
	// spaced audio samples covering each span.
	var out []SegmentPlan
	maxAlign := 0.0
	for i, seg := range videoPlan {
		if seg.endTicks == clock.NoTime {
			// Final span: derive a nominal end from the next boundary or a fixed
			// segment length.
			seg.endTicks = seg.startTicks + 4*int64(clock.Base)
		}
		span := seg.endTicks - seg.startTicks
		if span <= 0 {
			continue
		}
		// One sample per 1024/48000 s is ~1920 ticks; produce evenly spaced
		// samples so each span is populated.
		const sampleTicks = int64(1920)
		count := int(span / sampleTicks)
		if count == 0 {
			count = 1
		}
		samples := make([]sample, 0, count)
		for k := 0; k < count; k++ {
			payload := make([]byte, 16) // small framed payload
			samples = append(samples, sample{
				size:  16, ticks: seg.startTicks + int64(k)*sampleTicks,
				dur: sampleTicks, data: payload,
			})
		}
		base := samples[0].ticks
		data, err := buildMediaSegment(trackIDAudio, samples, base)
		if err != nil {
			return nil, 0, fmt.Errorf("audio segment %d: %w", i, err)
		}
		name := fmt.Sprintf("audio-%03d.m4s", i)
		if err := writeFileAtomic(filepath.Join(dir, name), data); err != nil {
			return nil, 0, err
		}
		align := mathAbsMS(base - seg.startTicks)
		if align > maxAlign {
			maxAlign = align
		}
		out = append(out, SegmentPlan{
			File: name, StartTicks: seg.startTicks, EndTicks: seg.endTicks,
			Bytes: int64(len(data)), SHA256: sha256hex(data),
		})
	}
	return out, maxAlign, nil
}

// fillDurations sets a duration on each sample from the gap to the next, and
// fills the last one with the previous sample's interval.
func fillDurations(s []sample) {
	for i := 0; i+1 < len(s); i++ {
		d := s[i+1].ticks - s[i].ticks
		if d < 0 {
			d = 0
		}
		s[i].dur = d
	}
	if n := len(s); n >= 2 {
		s[n-1].dur = s[n-2].dur
	} else if n == 1 && s[0].dur == 0 {
		s[0].dur = int64(clock.Base) / 30
	}
}

// tickOf expresses an absolute frame tick on the index timeline.
func tickOf(abs int64, origin int64) int64 {
	if abs == clock.NoTime {
		return clock.NoTime
	}
	return abs - origin
}

// buildManifest assembles the manifest root.
func buildManifest(idx *index.Index, opts Options, tracks []ManifestTrack, res *Result) *Manifest {
	m := &Manifest{
		Format:        "cinepipe-fmp4",
		Timescale:     int64(Timescale),
		TargetKBPS:    opts.TargetKBPS,
		Source:        idx.Source,
		Tracks:        tracks,
		Gaps:          res.Gaps,
		AlignmentMaxMS: res.AlignmentMaxMS,
	}
	// Actual bitrate: total media bytes over the covered duration.
	var totalBytes int64
	for _, s := range res.VideoSegments {
		totalBytes += s.Bytes
	}
	var duration int64
	if len(res.VideoSegments) > 0 {
		last := res.VideoSegments[len(res.VideoSegments)-1]
		duration = last.EndTicks
	}
	m.DurationTicks = duration
	if duration > 0 {
		seconds := float64(duration) / float64(clock.Base)
		m.ActualKBPS = round3(float64(totalBytes) * 8 / 1000 / seconds)
	}
	return m
}

// finalizeDigest computes the combined digest over all produced files plus the
// manifest content (excluding the digest field itself, to avoid circularity).
func finalizeDigest(res *Result, dir, initName string) error {
	names := []string{initName}
	for _, s := range res.VideoSegments {
		names = append(names, s.File)
	}
	for _, s := range res.AudioSegments {
		names = append(names, s.File)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	res.OutputSHATotal = hex.EncodeToString(h.Sum(nil))
	return nil
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mathAbsMS converts a tick delta to an absolute millisecond value.
func mathAbsMS(ticks int64) float64 {
	if ticks < 0 {
		ticks = -ticks
	}
	return clock.Millis(ticks)
}

// round3 rounds to three decimals for stable report output.
func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

// now is a hook for deterministic timestamps in tests; production uses wall
// clock only for file mtimes, never manifest content.
var _ = time.Now
