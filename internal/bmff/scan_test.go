package bmff_test

import (
	"bytes"
	"errors"
	"testing"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/bmff"
	"mediad/internal/fixture"
	"mediad/internal/model"
)

func scanBytes(t *testing.T, data []byte, sha string) *bmff.Index {
	t.Helper()
	r := bmff.NewReader("memory.mp4", bytes.NewReader(data), int64(len(data)))
	idx, err := r.Scan(sha)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return idx
}

func TestScanGeneratedFlatMP4(t *testing.T) {
	media, err := fixture.GenerateMedia(17, nil)
	if err != nil {
		t.Fatal(err)
	}
	idx := scanBytes(t, media.Data, media.SHA256)
	if len(idx.Tracks) != 4 {
		t.Fatalf("tracks=%d want 4", len(idx.Tracks))
	}
	wantSamples := []int{64, 8, 16, 8}
	codecs := []string{"avc1", "hvc1", "mp4a", "alac"}
	for i, tr := range idx.Tracks {
		if len(tr.Samples) != wantSamples[i] {
			t.Fatalf("track %d samples=%d want %d", tr.ID, len(tr.Samples), wantSamples[i])
		}
		if tr.Codec != codecs[i] {
			t.Fatalf("track %d codec=%q want %q", tr.ID, tr.Codec, codecs[i])
		}
		if len(tr.SampleEntry) == 0 {
			t.Fatalf("track %d missing sample entry/config", tr.ID)
		}
		if tr.Samples[0].Offset <= 0 || tr.Samples[0].Size == 0 {
			t.Fatalf("track %d bad first sample: %+v", tr.ID, tr.Samples[0])
		}
	}
	// ctts run [2002,0,1001,0] repeats for the first four samples.
	first := idx.Tracks[0].Samples[:4]
	wantPTS := []int64{2002, 1001, 3003, 3003}
	for i, want := range wantPTS {
		if first[i].PTS != want {
			t.Fatalf("pts[%d]=%d want %d (dts=%d)", i, first[i].PTS, want, first[i].DTS)
		}
	}
}

func TestTruncatedBoxReportsExactExpectedOffset(t *testing.T) {
	media, err := fixture.GenerateMedia(17, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := media.Data[:media.TruncateAt]
	r := bmff.NewReader("truncated.mp4", bytes.NewReader(bad), int64(len(bad)))
	_, err = r.Scan("bad")
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCorruptBox {
		t.Fatalf("expected corrupt_box, got %v", err)
	}
	if me.Offset != media.ExpectedBadBox || me.ExpectedOffset != media.ExpectedBadBox || me.DetectedOffset != media.ExpectedBadBox {
		t.Fatalf("offset=%d expected=%d detected=%d want %d", me.Offset, me.ExpectedOffset, me.DetectedOffset, media.ExpectedBadBox)
	}
}

func TestInvalidBoxLengthReportsHeaderOffset(t *testing.T) {
	data := []byte{0, 0, 0, 4, 'm', 'o', 'o', 'v'}
	r := bmff.NewReader("bad-length.mp4", bytes.NewReader(data), int64(len(data)))
	_, err := r.Children(nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCorruptBox {
		t.Fatalf("expected corrupt_box, got %v", err)
	}
	if me.Offset != 0 || me.BoxType != "moov" {
		t.Fatalf("error offset=%d box=%q", me.Offset, me.BoxType)
	}
}

func Test64BitBoxAndCO64(t *testing.T) {
	specs := fixture.DefaultTrackSpecs()
	specs = specs[:1]
	specs[0].Samples = 3
	specs[0].SampleSize = 16
	specs[0].UseCO64 = true
	media, err := fixture.GenerateMedia(9, specs)
	if err != nil {
		t.Fatal(err)
	}
	idx := scanBytes(t, media.Data, media.SHA256)
	if len(idx.Tracks[0].Samples) != 3 {
		t.Fatalf("samples=%d", len(idx.Tracks[0].Samples))
	}
	if idx.Tracks[0].Samples[2].Offset <= idx.Tracks[0].Samples[0].Offset {
		t.Fatalf("CO64 offsets not resolved: %+v", idx.Tracks[0].Samples)
	}

	large := bmff.NewBox("test").WithPayload([]byte("hello"))
	large.Force64 = true
	raw := large.Build()
	if raw[0:4][0] != 0 || raw[3] != 1 {
		t.Fatalf("missing 64-bit size marker: % x", raw[:8])
	}
	rr := bmff.NewReader("64.mp4", bytes.NewReader(raw), int64(len(raw)))
	boxes, err := rr.Children(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 || boxes[0].HeaderSize != 16 || boxes[0].Size != int64(len(raw)) {
		t.Fatalf("bad 64-bit header: %+v", boxes)
	}
}

func TestGoMP4CrossChecksGeneratedStructure(t *testing.T) {
	media, err := fixture.GenerateMedia(17, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(media.Data)
	_, err = gomp4.ReadBoxStructure(r, func(h *gomp4.ReadHandle) (interface{}, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go-mp4 rejected generated box structure: %v", err)
	}
}
