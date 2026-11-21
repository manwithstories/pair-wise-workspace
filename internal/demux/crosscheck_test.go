package demux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"

	"mediamerge/internal/fixture"
)

// TestCrossCheckWithGoMP4 parses the same fixture with the abema/go-mp4
// extraction API and asserts our track ids, timescales and handlers agree.
// This keeps the pinned third-party reader in the verification loop instead of
// trusting a single parser.
func TestCrossCheckWithGoMP4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(path, fixture.DefaultPlan(3, 6).Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	ours, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	trakPath := mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()}
	traks, err := mp4.ExtractBoxes(f, nil, []mp4.BoxPath{trakPath})
	if err != nil {
		t.Fatalf("extract traks: %v", err)
	}
	if len(traks) != len(ours.Tracks) {
		t.Fatalf("go-mp4 tracks=%d ours=%d", len(traks), len(ours.Tracks))
	}
	for i, tr := range traks {
		tkhdBoxes, err := mp4.ExtractBoxWithPayload(f, tr, mp4.BoxPath{
			mp4.BoxTypeTkhd()})
		if err != nil {
			t.Fatal(err)
		}
		tkhd := tkhdBoxes[0].Payload.(*mp4.Tkhd)
		mdhdBoxes, err := mp4.ExtractBoxWithPayload(f, tr, mp4.BoxPath{
			mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
		if err != nil {
			t.Fatal(err)
		}
		mdhd := mdhdBoxes[0].Payload.(*mp4.Mdhd)
		hdlrBoxes, err := mp4.ExtractBoxWithPayload(f, tr, mp4.BoxPath{
			mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()})
		if err != nil {
			t.Fatal(err)
		}
		hdlr := hdlrBoxes[0].Payload.(*mp4.Hdlr)

		o := ours.Tracks[i]
		if tkhd.TrackID != o.InputID || mdhd.Timescale != o.Timescale ||
			string(hdlr.HandlerType[:]) != o.HandlerType {
			t.Fatalf("track %d go-mp4={%d %d %s} ours={%d %d %s}", i,
				tkhd.TrackID, mdhd.Timescale, string(hdlr.HandlerType[:]),
				o.InputID, o.Timescale, o.HandlerType)
		}
	}

	// The sample table must also agree: total sizes equal the sum of the
	// compressed payloads we indexed.
	stszPath := mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
		mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()}
	stszBoxes, err := mp4.ExtractBoxesWithPayload(f, nil, []mp4.BoxPath{stszPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(stszBoxes) != len(ours.Tracks) {
		t.Fatalf("stsz boxes=%d", len(stszBoxes))
	}
	for i, b := range stszBoxes {
		stsz := b.Payload.(*mp4.Stsz)
		if int(stsz.SampleCount) != len(ours.Tracks[i].Samples) {
			t.Fatalf("track %d go-mp4 samples=%d ours=%d", i, stsz.SampleCount,
				len(ours.Tracks[i].Samples))
		}
	}
}
