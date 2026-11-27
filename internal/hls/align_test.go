package hls

import (
	"strings"
	"testing"
)

func mkVariant(name string, bandwidth uint32, cuts ...int64) Variant {
	return Variant{
		Name: name, Bandwidth: bandwidth, Resolution: "1280x720",
		Codecs:      "avc1.4d401f",
		Boundaries:  cuts,
		SegmentFile: func(seq int) string { return name + "/seg" + itoa(seq) + ".m4s" },
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func TestAlignSharedBoundaries(t *testing.T) {
	// Two variants already sharing cut points at 0, 90000, 180000 ticks.
	v1 := mkVariant("hi", 2_000_000, 0, 90000, 180000)
	v2 := mkVariant("lo", 800_000, 0, 90000, 180000)
	bounds, err := Align([]Variant{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounds) != 2 {
		t.Fatalf("bounds = %d, want 2", len(bounds))
	}
	if bounds[0].Start != 0 || bounds[0].End != 90000 {
		t.Fatalf("first bound wrong: %+v", bounds[0])
	}
	if Skew(bounds, v1.Boundaries) != 0 || Skew(bounds, v2.Boundaries) != 0 {
		t.Fatal("identical cuts must produce zero skew")
	}
}

func TestAlignWithinOneFrameSnaps(t *testing.T) {
	// Variant boundaries differ by less than one frame; they must merge.
	v1 := mkVariant("hi", 2_000_000, 0, 90000, 180000)
	v2 := mkVariant("lo", 800_000, 0, 90010, 180000) // 10 ticks < 3000
	bounds, err := Align([]Variant{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounds) != 2 {
		t.Fatalf("bounds = %d, want 2 after snapping", len(bounds))
	}
	// Snapped to the earlier tick (90000), and measured skew stays within a frame.
	if bounds[0].End != 90000 {
		t.Fatalf("snapped end = %d, want 90000", bounds[0].End)
	}
	if got := Skew(bounds, v2.Boundaries); got > FrameTicks {
		t.Fatalf("skew %d exceeds one frame %d", got, FrameTicks)
	}
}

func TestAlignRejectsNegativeBoundary(t *testing.T) {
	v := mkVariant("bad", 1, 0, -5, 90000)
	_, err := Align([]Variant{v})
	if err == nil {
		t.Fatal("negative boundary must fail")
	}
	if !strings.Contains(err.Error(), "negative-duration") {
		t.Fatalf("err = %v, want negative-duration fault", err)
	}
}

func TestAssignEventBoundaryGoesToLaterSegment(t *testing.T) {
	bounds := []Bound{
		{Seq: 0, Start: 0, End: 90000},
		{Seq: 1, Start: 90000, End: 180000},
	}
	// An event exactly on the shared cut belongs to the later segment.
	seq, ok := AssignEvent(bounds, 90000)
	if !ok || seq != 1 {
		t.Fatalf("boundary event -> seq %d ok=%v, want 1", seq, ok)
	}
	// Just before the cut belongs to the first.
	seq, ok = AssignEvent(bounds, 89999)
	if !ok || seq != 0 {
		t.Fatalf("pre-boundary event -> seq %d, want 0", seq)
	}
	// Outside the window is unmappable.
	if _, ok := AssignEvent(bounds, 180000); ok {
		t.Fatal("event at/after final end must be unmappable")
	}
}

func TestBuildPlaylistsDeterministic(t *testing.T) {
	v1 := mkVariant("hi", 2_000_000, 0, 90000, 180000)
	v2 := mkVariant("lo", 800_000, 0, 90000, 180000)
	bounds, _ := Align([]Variant{v1, v2})
	audio := []MediaRendition{{
		GroupID: "audio-stereo-48000", Name: "stereo", URI: "audio/stereo.m3u8",
		SampleRate: 48000, Channels: 2, Default: true,
	}}
	set, err := BuildPlaylists([]Variant{v1, v2}, bounds, audio)
	if err != nil {
		t.Fatal(err)
	}
	master1 := set.EncodeMaster()
	master2 := set.EncodeMaster()
	if string(master1) != string(master2) {
		t.Fatal("master playlist encoding is not deterministic")
	}
	if !strings.Contains(string(master1), "#EXTM3U") ||
		!strings.Contains(string(master1), "BANDWIDTH=2000000") {
		t.Fatalf("master missing expected tags:\n%s", master1)
	}
	if !strings.Contains(string(master1), "TYPE=AUDIO") ||
		!strings.Contains(string(master1), `CHANNELS="2"`) {
		t.Fatalf("master missing audio rendition:\n%s", master1)
	}
	media, err := set.EncodeMedia("hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(media), "#EXT-X-ENDLIST") {
		t.Fatalf("VOD media playlist must end with ENDLIST:\n%s", media)
	}
	if c := strings.Count(string(media), "#EXTINF"); c != 2 {
		t.Fatalf("EXTINF count = %d, want 2", c)
	}
	if pl := set.MediaByName["hi"]; pl.TargetDuration < 1 {
		t.Fatalf("TargetDuration = %d, must be >= 1", pl.TargetDuration)
	}
	if !strings.Contains(string(media), "#EXT-X-TARGETDURATION:1") {
		t.Fatalf("sub-second segments must round target duration up to 1:\n%s", media)
	}
	uris := set.SegmentURIs()
	if len(uris) == 0 || uris[0] > uris[len(uris)-1] {
		t.Fatalf("segment URIs not sorted: %v", uris)
	}
}
