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

func TestFragmentedMoofTFDTAndTRUN(t *testing.T) {
	entry := bmff.NewBox("avc1").WithPayload(make([]byte, 8))
	entryBytes := entry.Build()
	stsdPayload := make([]byte, 4+len(entryBytes))
	binaryBigEndianPutU32(stsdPayload[0:4], 1)
	copy(stsdPayload[4:], entryBytes)
	stsd := bmff.NewFullBox("stsd", 0, 0, stsdPayload)
	stbl := bmff.NewBox("stbl").Add(stsd)

	tkhdPayload := make([]byte, 80)
	binaryBigEndianPutU32(tkhdPayload[8:12], 7)
	mdhdPayload := make([]byte, 20)
	binaryBigEndianPutU32(mdhdPayload[8:12], 1000)
	hdlrPayload := make([]byte, 21)
	copy(hdlrPayload[4:8], "vide")
	trak := bmff.NewBox("trak").Add(
		bmff.NewFullBox("tkhd", 0, 7, tkhdPayload),
		bmff.NewBox("mdia").Add(
			bmff.NewFullBox("mdhd", 0, 0, mdhdPayload),
			bmff.NewFullBox("hdlr", 0, 0, hdlrPayload),
			bmff.NewBox("minf").Add(stbl),
		),
	)
	trexPayload := make([]byte, 20)
	binaryBigEndianPutU32(trexPayload[0:4], 7)
	moov := bmff.NewBox("moov").Add(trak, bmff.NewBox("mvex").Add(bmff.NewFullBox("trex", 0, 0, trexPayload)))

	sampleData := [][]byte{[]byte("aaaa"), []byte("bbbbbb"), []byte("cccccccc")}
	trunPayload := make([]byte, 8+12*len(sampleData))
	binaryBigEndianPutU32(trunPayload[0:4], uint32(len(sampleData)))
	// data offset filled after measuring moof
	pos := 8
	for _, s := range []struct {
		dur, size uint32
		cts       int32
	}{{100, 4, 0}, {200, 6, 10}, {300, 8, -5}} {
		binaryBigEndianPutU32(trunPayload[pos:], s.dur)
		binaryBigEndianPutU32(trunPayload[pos+4:], s.size)
		binaryBigEndianPutU32(trunPayload[pos+8:], uint32(s.cts))
		pos += 12
	}
	trun := bmff.NewFullBox("trun", 1, 0x000001|0x000100|0x000200|0x000800, trunPayload)
	tfhdPayload := make([]byte, 4)
	binaryBigEndianPutU32(tfhdPayload, 7)
	tfdtPayload := make([]byte, 8)
	binaryBigEndianPutU64(tfdtPayload, 100)
	mfhd := bmff.NewFullBox("mfhd", 0, 0, []byte{0, 0, 0, 1})
	traf := bmff.NewBox("traf").Add(
		bmff.NewFullBox("tfhd", 0, 0x020000, tfhdPayload),
		bmff.NewFullBox("tfdt", 1, 0, tfdtPayload),
		trun,
	)
	moof := bmff.NewBox("moof").Add(mfhd, traf)
	moofBytes := moof.Build()
	binaryBigEndianPutU32(trunPayload[4:8], uint32(len(moofBytes)+8))
	moofBytes = moof.Build()

	var mdatPayload []byte
	for _, b := range sampleData {
		mdatPayload = append(mdatPayload, b...)
	}
	mdat := bmff.NewBox("mdat").WithPayload(mdatPayload).Build()
	data := append(moov.Build(), moofBytes...)
	data = append(data, mdat...)

	idx := scanBytes(t, data, "fragmented")
	if !idx.Fragmented || len(idx.Tracks) != 1 || idx.Tracks[0].ID != 7 {
		t.Fatalf("bad fragmented index: %+v", idx)
	}
	got := idx.Tracks[0].Samples
	if len(got) != 3 {
		t.Fatalf("samples=%d want3", len(got))
	}
	wantDTS := []int64{100, 200, 400}
	wantPTS := []int64{100, 210, 395}
	for i := range got {
		if got[i].DTS != wantDTS[i] || got[i].PTS != wantPTS[i] {
			t.Fatalf("sample %d dts=%d pts=%d want %d/%d", i, got[i].DTS, got[i].PTS, wantDTS[i], wantPTS[i])
		}
	}
	if got[0].Offset != int64(len(moov.Build())+len(moofBytes)+8) {
		t.Fatalf("first offset=%d", got[0].Offset)
	}
}

func binaryBigEndianPutU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func binaryBigEndianPutU64(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}
