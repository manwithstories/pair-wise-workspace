package fixtures

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// buildClean writes two on-time camera assets with wrap-prone 33-bit clocks.
// The audio starts at a non-zero PTS and the edits include a cut in the middle
// of a frame so the sample alignment path is exercised.
func buildClean(spec Spec, set *Set) error {
	const start = int64(96000) // 2 s into the camera clock
	files := []struct {
		camera string
		track  string
		seed   int64
		start  int64
	}{
		{"camA", "a1", spec.Seed, start},
		{"camB", "a1", spec.Seed + 17, start + 2400},
	}
	var manifest, edits strings.Builder
	for i, f := range files {
		path := filepath.Join(spec.Root, "media", f.camera+".mov")
		cfg := avConfig{
			Seed: f.seed, Width: 160, Height: 90, VideoFrames: 24,
			VideoStartMS: 1200, VideoDeltasMS: chunkPlan(f.seed, 10),
			AudioRate: 48000, AudioStart: f.start, AudioSamples: 48000 * 6,
			AudioLayout: astiav.ChannelLayoutStereo,
		}
		if err := writeAVMOV(path, cfg); err != nil {
			return err
		}
		set.Media = append(set.Media, path)
		hostBase := int64(1_700_000_000_000_000) + int64(i)*1_000_000
		manifest.WriteString(fmt.Sprintf(
			`{"camera":%q,"file":"media/%s.mov","track":%q,"timebase":"1/48000","wrap_bits":33,"stream_index":1,"layout":"stereo","channels":2,"sample_rate":48000,"frame_rate":"25/1","anchors":[{"host_us":%d,"media_ticks":%d},{"host_us":%d,"media_ticks":%d}]}`+"\n",
			f.camera, f.camera, f.track, hostBase, f.start, hostBase+5_000_000, f.start+5*48000))
	}
	// Cuts: one aligned, one mid-frame, one near the end of the asset.
	edits.WriteString(`{"camera":"camA","track":"a1","in":96000,"out":144000,"seq":0,"label":"intro"}` + "\n")
	edits.WriteString(`{"camera":"camB","track":"a1","in":99520,"out":147648,"seq":1,"label":"verse"}` + "\n")
	edits.WriteString(`{"camera":"camA","track":"a1","in":240000,"out":288000,"seq":2,"label":"outro"}` + "\n")
	if err := writeText(set.Manifest, manifest.String()); err != nil {
		return err
	}
	if err := writeText(set.Edits, edits.String()); err != nil {
		return err
	}
	set.Notes = append(set.Notes, "clean: nonzero PTS, VFR video, sparse keyframes")
	return nil
}

// buildLateWrap writes a camera whose 33-bit clock wraps inside the recording
// and whose final edit arrives after the watermark has moved past it.
func buildLateWrap(spec Spec, set *Set) error {
	const period = int64(1) << 33
	const start = period - 3*48000 // 3 s before the wrap
	path := filepath.Join(spec.Root, "media", "camWrap.mov")
	cfg := avConfig{
		Seed: spec.Seed + 41, Width: 160, Height: 90, VideoFrames: 24,
		VideoStartMS: 3000, VideoDeltasMS: chunkPlan(spec.Seed, 10),
		AudioRate: 48000, AudioStart: start, AudioSamples: 48000 * 12,
		AudioLayout: astiav.ChannelLayoutStereo,
	}
	if err := writeAVMOV(path, cfg); err != nil {
		return err
	}
	set.Media = append(set.Media, path)
	hostBase := int64(1_700_000_500_000_000)
	// Anchor media ticks exceed 33 bits: the host-side unwrap already folded the
	// wrap and the drift fit operates in the unwrapped domain. A small positive
	// drift makes the camera run fast by a few ppm.
	manifest := fmt.Sprintf(
		`{"camera":"camWrap","file":"media/camWrap.mov","track":"a1","timebase":"1/48000","wrap_bits":33,"stream_index":1,"layout":"stereo","channels":2,"sample_rate":48000,"anchors":[{"host_us":%d,"media_ticks":%d},{"host_us":%d,"media_ticks":%d}]}`+"\n",
		hostBase, start, hostBase+6_998_740, start+7*48000)
	// The audio spans [period-144000, period+240000). Edit ticks are the
	// wrapped 33-bit values the DIT recorded; two sit at or beyond the wrap
	// point, so the clock stage must lift them into the unwrapped domain.
	// seq 2 is delivered last but sorts first on the timeline: a late arrival
	// that forces the timeline to replay the already-accepted events.
	edits := strings.Join([]string{
		`{"camera":"camWrap","track":"a1","in":8589944592,"out":8589964592,"seq":0,"label":"post-wrap"}`,  // [period+10000, period+30000)
		`{"camera":"camWrap","track":"a1","in":8589974592,"out":8589994592,"seq":1,"label":"tail"}`,       // [period+340000, period+360000)
		`{"camera":"camWrap","track":"a1","in":8589850592,"out":8589870592,"seq":2,"label":"late-cross"}`, // [period-84000, period-64000), delivered last
	}, "\n") + "\n"
	if err := writeText(set.Manifest, manifest); err != nil {
		return err
	}
	if err := writeText(set.Edits, edits); err != nil {
		return err
	}
	set.Notes = append(set.Notes, "late-wrap: 33-bit wrap crossing and late delivery")
	return nil
}

// buildTruncated writes an asset whose final audio packet is short, so the
// demuxer flags it corrupt. The cut list includes that packet's time range;
// the pipeline must record the damaged track/time and skip the interval.
func buildTruncated(spec Spec, set *Set) error {
	path := filepath.Join(spec.Root, "media", "camBroken.mov")
	cfg := avConfig{
		Seed: spec.Seed + 7, Width: 160, Height: 90, VideoFrames: 16,
		VideoStartMS: 0, VideoDeltasMS: chunkPlan(spec.Seed+1, 10),
		AudioRate: 48000, AudioStart: 48000, AudioSamples: 48000 * 4,
		AudioLayout: astiav.ChannelLayoutStereo,
	}
	if err := writeAVMOV(path, cfg); err != nil {
		return err
	}
	if err := truncateMP4Sample(path); err != nil {
		return err
	}
	set.Media = append(set.Media, path)
	hostBase := int64(1_700_001_000_000_000)
	manifest := fmt.Sprintf(
		`{"camera":"camBroken","file":"media/camBroken.mov","track":"a1","timebase":"1/48000","wrap_bits":33,"stream_index":1,"layout":"stereo","channels":2,"sample_rate":48000,"anchors":[{"host_us":%d,"media_ticks":48000},{"host_us":%d,"media_ticks":%d}]}`+"\n",
		hostBase, hostBase+3_000_000, 48000+3*48000)
	// The last cut covers the truncated tail.
	edits := strings.Join([]string{
		`{"camera":"camBroken","track":"a1","in":48000,"out":72000,"seq":0,"label":"head"}`,
		`{"camera":"camBroken","track":"a1","in":216000,"out":240000,"seq":1,"label":"damaged-tail"}`,
	}, "\n") + "\n"
	if err := writeText(set.Manifest, manifest); err != nil {
		return err
	}
	if err := writeText(set.Edits, edits); err != nil {
		return err
	}
	set.Notes = append(set.Notes, "truncated: short final packet must be skipped, not fatal")
	return nil
}

// buildInvalidTB writes a manifest whose time base is a zero denominator. The
// parser must reject it with INVALID_TIMEBASE and never open the media.
func buildInvalidTB(spec Spec, set *Set) error {
	manifest := `{"camera":"camBad","file":"media/camBad.mov","track":"a1","timebase":"1/0","wrap_bits":33,"stream_index":1,"layout":"stereo","channels":2,"sample_rate":48000,"anchors":[{"host_us":1700000000000000,"media_ticks":0}]}` + "\n"
	edits := `{"camera":"camBad","track":"a1","in":0,"out":48000,"seq":0}` + "\n"
	if err := writeText(set.Manifest, manifest); err != nil {
		return err
	}
	if err := writeText(set.Edits, edits); err != nil {
		return err
	}
	set.ExpectError = media.ErrInvalidTimebase
	set.Notes = append(set.Notes, "invalid-base: zero denominator rejected before media I/O")
	return nil
}
