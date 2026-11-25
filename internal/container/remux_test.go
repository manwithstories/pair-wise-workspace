package container

import (
	"bytes"
	"encoding/binary"
	"testing"

	"archivectl/internal/timeline"
)

func seiSample() []byte {
	b := []byte{0, 0, 0, 1, 0x06, 0x05, 0x04, 'A', 'R', 'C', 'H'}
	b = append(b, 0, 0, 0, 1, 0x0c)
	b = append(b, bytes.Repeat([]byte{0x5a}, 4980-len(b))...)
	return b
}

func testAsset() *Asset {
	videoData := [][]byte{seiSample(), bytes.Repeat([]byte{0x10}, 5000), bytes.Repeat([]byte{0x20}, 5000)}
	videoSamples := make([]Sample, 3)
	for i, d := range videoData {
		videoSamples[i] = Sample{Index: i, Data: d, Size: len(d), DTS: int64(i * 3000), PTS: int64(i*3000 + 900), Duration: 3000}
	}
	subSamples := []Sample{
		{Index: 0, Data: []byte("Opening titles"), Size: 14, DTS: 0, PTS: 0, Duration: 2000},
		{Index: 1, Data: []byte("Interview begins"), Size: 16, DTS: 2000, PTS: 2000, Duration: 2000},
	}
	chapterSamples := []Sample{{Index: 0, Data: []byte("Tape 1 / Side A"), Size: 15, DTS: 0, PTS: 0, Duration: 10000}}
	id3 := append([]byte("ID3\x04\x00\x00"), bytes.Repeat([]byte{0}, 32)...)
	metaSamples := []Sample{{Index: 0, Data: id3, Size: len(id3), DTS: 0, PTS: 500, Duration: 1000}}
	emsgBox := BuildEMSG("urn:archive:cue", "oral-history", 1000, 1500, 500, 7, []byte("chapter marker"))
	return &Asset{
		Tracks: []Track{
			{ID: 1, Kind: KindVideo, Codec: "avc1", Timescale: 90000, Samples: videoSamples, References: []uint32{2}},
			{ID: 2, Kind: KindSubtitle, Codec: "tx3g", Timescale: 1000, Samples: subSamples},
			{ID: 3, Kind: KindChapters, Codec: "chpl", Timescale: 1000, Samples: chapterSamples},
			{ID: 4, Kind: KindMetadata, Codec: "ID32", Timescale: 1000, Origin: 100, Samples: metaSamples},
		},
		Events: []Event{{Kind: EventEMSG, TrackID: 0, TimeNS: 1_500_000_000, Timescale: 1000, RawTime: 1500, RawBox: emsgBox}},
	}
}

func TestRemuxPreservesSampleBytesAndTimestamps(t *testing.T) {
	src := testAsset()
	remux, err := RemuxAsset(src)
	if err != nil {
		t.Fatal(err)
	}
	again, err := RemuxAsset(src)
	if err != nil {
		t.Fatal(err)
	}
	if remux.OutputHash != again.OutputHash || !bytes.Equal(remux.Bytes, again.Bytes) {
		t.Fatal("remux output is not deterministic")
	}
	out, err := ParseBytes("out.mp4", remux.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyAssets(src, out)
	if err != nil {
		t.Fatalf("verify failed: %v report=%+v", err, report)
	}
	if report.PayloadHashMismatch != 0 || report.TimestampMismatch != 0 {
		t.Fatalf("mismatch report: %+v", report)
	}
	for _, st := range src.Tracks {
		ot, _ := out.TrackByID(st.ID)
		for i := range st.Samples {
			if !bytes.Equal(st.Samples[i].Data, ot.Samples[i].Data) {
				t.Fatalf("track %d sample %d payload changed", st.ID, i)
			}
			if st.Samples[i].PTS != ot.Samples[i].PTS || st.Samples[i].DTS != ot.Samples[i].DTS {
				t.Fatalf("track %d sample %d timestamps changed", st.ID, i)
			}
		}
	}
	events := CollectEvents(out)
	found := map[string]bool{}
	for _, e := range events {
		found[e.Kind] = true
	}
	for _, kind := range []string{EventSubtitle, EventChapter, EventID3, EventSEI, EventEMSG} {
		if !found[kind] {
			t.Fatalf("missing event kind %s in %+v", kind, found)
		}
	}
}

func TestTruncatedBoxReportsOffset(t *testing.T) {
	res, err := RemuxAsset(testAsset())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseBytes("truncated.mp4", res.Bytes[:len(res.Bytes)-37])
	if err == nil {
		t.Fatal("expected truncation error")
	}
	var te *timeline.Error
	if !asTimeline(err, &te) || te.Code != timeline.CodeCorruptBox || te.Offset < 0 {
		t.Fatalf("expected corrupt box with offset, got %v", err)
	}
}

func TestIllegalTrackReference(t *testing.T) {
	src := testAsset()
	src.Tracks[0].References = []uint32{999}
	b, err := BuildMP4(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseBytes("bad-ref.mp4", b)
	if !timeline.IsCode(err, timeline.CodeIllegalReference) {
		t.Fatalf("expected illegal reference, got %v", err)
	}
}

func TestNegativeDurationReportsTrackAndOffset(t *testing.T) {
	track := Track{ID: 42, Kind: KindVideo, Timescale: 90000, Samples: []Sample{{
		Index: 0, Data: []byte("x"), Size: 1, DTS: 0, PTS: 0, Duration: 1,
	}}}
	box := EncodeTrackIndex(track)
	// One record starts at payload offset 40; duration is its third int64 and
	// the box has an 8-byte header, so the field begins at byte 64.
	binary.BigEndian.PutUint64(box[64:72], ^uint64(0))
	_, err := ParseTrackIndex(box[8:], 0)
	var te *timeline.Error
	if !asTimeline(err, &te) || te.Code != timeline.CodeNegativeDuration {
		t.Fatalf("expected negative_duration, got %v", err)
	}
	if te.TrackID != 42 || te.Offset != 64 {
		t.Fatalf("location track=%d offset=%d, want 42/64", te.TrackID, te.Offset)
	}
}
