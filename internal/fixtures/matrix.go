package fixtures

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// CaseMatrix exercises explicit channel mixing: a 5.1 side source folded
// through the pinned matrix, and a mono source that must be centred.
const CaseMatrix = "matrix"

// CaseBadLayout is a manifest that names a layout with no explicit matrix.
const CaseBadLayout = "bad-layout"

func buildMatrix(spec Spec, set *Set) error {
	const start = int64(48000)
	type src struct {
		camera string
		layout astiav.ChannelLayout
		name   string
		chans  int
		matrix string
	}
	sources := []src{
		{"camMono", astiav.ChannelLayoutMono, "mono", 1, ""},
		{"cam51", astiav.ChannelLayout5Point1, "5.1(side)", 6, ""},
	}
	var manifest, edits strings.Builder
	for i, s := range sources {
		path := filepath.Join(spec.Root, "media", s.camera+".mov")
		cfg := avConfig{
			Seed: spec.Seed + int64(i)*13, Width: 160, Height: 90, VideoFrames: 12,
			VideoStartMS: 0, VideoDeltasMS: chunkPlan(spec.Seed, 10),
			AudioRate: 48000, AudioStart: start, AudioSamples: 48000 * 24,
			AudioLayout: s.layout,
		}
		if err := writeAVMOV(path, cfg); err != nil {
			return err
		}
		set.Media = append(set.Media, path)
		hostBase := int64(1_700_002_000_000_000) + int64(i)*1_000_000
		manifest.WriteString(fmt.Sprintf(
			`{"camera":%q,"file":"media/%s.mov","track":"a1","timebase":"1/48000","wrap_bits":33,"stream_index":1,"layout":%q,"channels":%d,"sample_rate":48000,"anchors":[{"host_us":%d,"media_ticks":%d},{"host_us":%d,"media_ticks":%d}]}`+"\n",
			s.camera, s.camera, s.name, s.chans, hostBase, start, hostBase+3_000_000, start+3*48000))
	}
	edits.WriteString(`{"camera":"camMono","track":"a1","in":48000,"out":624000,"seq":0,"label":"mono-centred"}` + "\n")
	edits.WriteString(`{"camera":"cam51","track":"a1","in":48000,"out":624000,"seq":1,"label":"surround-fold"}` + "\n")
	if err := writeText(set.Manifest, manifest.String()); err != nil {
		return err
	}
	if err := writeText(set.Edits, edits.String()); err != nil {
		return err
	}
	set.Notes = append(set.Notes, "matrix: mono centred, 5.1 folded through an explicit matrix")
	return nil
}

// writeMatrixCase emits a manifest whose layout has no explicit matrix, so the
// pipeline rejects it as UNSUPPORTED_LAYOUT before rendering.
func buildBadLayout(spec Spec, set *Set) error {
	path := filepath.Join(spec.Root, "media", "camOdd.mov")
	cfg := avConfig{
		Seed: spec.Seed, Width: 160, Height: 90, VideoFrames: 8,
		VideoStartMS: 0, VideoDeltasMS: chunkPlan(spec.Seed, 10),
		AudioRate: 48000, AudioStart: 48000, AudioSamples: 48000 * 2,
		AudioLayout: astiav.ChannelLayoutQuad,
	}
	if err := writeAVMOV(path, cfg); err != nil {
		return err
	}
	set.Media = append(set.Media, path)
	manifest := `{"camera":"camOdd","file":"media/camOdd.mov","track":"a1","timebase":"1/48000","wrap_bits":33,"stream_index":1,"layout":"loudspeaker-7","channels":4,"sample_rate":48000,"anchors":[{"host_us":1700003000000000,"media_ticks":48000}]}` + "\n"
	edits := `{"camera":"camOdd","track":"a1","in":48000,"out":96000,"seq":0}` + "\n"
	if err := writeText(set.Manifest, manifest); err != nil {
		return err
	}
	if err := writeText(set.Edits, edits); err != nil {
		return err
	}
	set.ExpectError = media.ErrUnsupported
	set.Notes = append(set.Notes, "bad-layout: unknown layout must be rejected, not guessed")
	return nil
}
