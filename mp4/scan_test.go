package mp4_test

// These tests exercise the streaming scanner and sample-table indexer against
// byte-level, hand-built MP4 fixtures, so they never need ffmpeg.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
)

// box wraps a payload in an ISO-BMFF length-prefixed box.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}
func fullBox(typ string, version byte, payload []byte) []byte {
	// Allocate a fresh backing array: callers pass slices that may still have
	// spare capacity, and appending into that spare space would later be
	// overwritten when an outer box wraps this payload.
	hdr := []byte{version, 0, 0, 0}
	out := make([]byte, 0, len(hdr)+len(payload))
	out = append(out, hdr...)
	out = append(out, payload...)
	return box(typ, out)
}

// buildMP4 assembles a minimal but structurally valid single-track MP4.
// timescale 0 / cttsRollback / truncateAt are corruption switches.
func buildMP4(t *testing.T, timescale uint32, cttsRollback bool) []byte {
	t.Helper()
	const n = 6

	// mvhd (v0) payload: version/flags(4) + creation(4) + modification(4) +
	// timescale(4) + duration(4) + reserved/pad to 96 bytes.
	mvhdPayload := make([]byte, 96)
	binary.BigEndian.PutUint32(mvhdPayload[8:12], 1000)
	binary.BigEndian.PutUint32(mvhdPayload[12:16], n*10)
	mvhd := fullBox("mvhd", 0, mvhdPayload)

	// tkhd (v0) minimal 84 bytes.
	tkhd := fullBox("tkhd", 0, make([]byte, 80))

	// mdhd: creation(4) modification(4) timescale(4) duration(4) lang(2)+quality(2)
	mdhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdPayload[8:12], timescale)
	binary.BigEndian.PutUint32(mdhdPayload[12:16], n*10)
	mdhd := fullBox("mdhd", 0, mdhdPayload)

	// hdlr field area (fullBox prepends the 4-byte version/flags): pre_defined
	// (4) then handler_type (4) "vide".
	hdlrPayload := make([]byte, 24)
	copy(hdlrPayload[4:8], "vide")
	hdlr := fullBox("hdlr", 0, hdlrPayload)

	// stsd field area (fullBox prepends the 4-byte version/flags):
	// entry_count(4) + sample entry size(4) + codec type 4CC. After fullBox
	// the codec lands at absolute payload offset 12, matching ISO-BMFF.
	stsdPayload := make([]byte, 12)
	binary.BigEndian.PutUint32(stsdPayload[0:4], 1)  // entry_count
	binary.BigEndian.PutUint32(stsdPayload[4:8], 16) // sample entry size
	copy(stsdPayload[8:12], "avc1")
	stsd := fullBox("stsd", 0, stsdPayload)

	// stts: one run, n samples, 10 ticks each.
	sttsPayload := make([]byte, 8)
	binary.BigEndian.PutUint32(sttsPayload[0:4], 1)
	binary.BigEndian.PutUint32(sttsPayload[4:8], n)
	sttsPayload = append(sttsPayload, 0, 0, 0, 10)
	stts := fullBox("stts", 0, sttsPayload)

	// stsz: uniform=0, count=n, n size entries of 100 bytes.
	stszPayload := make([]byte, 8)
	binary.BigEndian.PutUint32(stszPayload[4:8], n)
	for i := 0; i < n; i++ {
		stszPayload = append(stszPayload, 0, 0, 0, 100)
	}
	stsz := fullBox("stsz", 0, stszPayload)

	// stsc: single run (first_chunk=1, spc=1).
	stscPayload := []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0}
	stsc := fullBox("stsc", 0, stscPayload)

	// stco offsets get patched after layout is known. Embed a unique marker
	// (entry count followed by a sentinel first entry) so the patch location
	// can be found inside the nested box tree without fragile offset math.
	stcoSentinel := uint32(0xDEADBEEF)
	stcoPayload := make([]byte, 4+4*n)
	binary.BigEndian.PutUint32(stcoPayload[0:4], n)
	binary.BigEndian.PutUint32(stcoPayload[4:8], stcoSentinel)
	stco := fullBox("stco", 0, stcoPayload)

	// stss: samples 1 and 4 are sync points.
	stssPayload := []byte{0, 0, 0, 2, 0, 0, 0, 1, 0, 0, 0, 4}
	stss := fullBox("stss", 0, stssPayload)

	stbl := box("stbl", bytes.Join([][]byte{stsd, stts, stsz, stsc, stco, stss}, nil))
	minf := box("minf", stbl)
	mdia := box("mdia", bytes.Join([][]byte{mdhd, hdlr, minf}, nil))
	trak := box("trak", bytes.Join([][]byte{tkhd, mdia}, nil))
	moov := box("moov", bytes.Join([][]byte{mvhd, trak}, nil))

	mdat := box("mdat", make([]byte, n*100))
	file := bytes.Join([][]byte{moov, mdat}, nil)

	// Patch chunk offsets: locate the stco entry table by its sentinel, then
	// overwrite every entry. The mdat payload starts at len(moov)+8.
	mdatOff := len(moov)
	sentinel := make([]byte, 4)
	binary.BigEndian.PutUint32(sentinel, 0xDEADBEEF)
	patchBase := bytes.Index(file, sentinel)
	if patchBase < 0 {
		t.Fatal("stco sentinel not found in assembled fixture")
	}
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint32(file[patchBase+4*i:patchBase+4*i+4],
			uint32(mdatOff+8+i*100))
	}

	if cttsRollback {
		// Insert a v1 (signed) ctts box that drags every sample far into the
		// past relative to decode order, exceeding any bounded B-frame window.
		cttsPayload := make([]byte, 8)
		binary.BigEndian.PutUint32(cttsPayload[0:4], 1) // one run
		binary.BigEndian.PutUint32(cttsPayload[4:8], n)
		for i := 0; i < n; i++ {
			cttsPayload = append(cttsPayload, 0, 0, 0, 0)
			signed := int32(-100000)
			binary.BigEndian.PutUint32(cttsPayload[len(cttsPayload)-4:], uint32(signed))
		}
		ctts := fullBox("ctts", 1, cttsPayload)
		// Rebuild stbl/minf/mdia/trak/moov with ctts inserted.
		stbl2 := box("stbl", bytes.Join([][]byte{stsd, stts, stsz, stsc, stco, stss, ctts}, nil))
		minf2 := box("minf", stbl2)
		mdia2 := box("mdia", bytes.Join([][]byte{mdhd, hdlr, minf2}, nil))
		trak2 := box("trak", bytes.Join([][]byte{tkhd, mdia2}, nil))
		moov2 := box("moov", bytes.Join([][]byte{mvhd, trak2}, nil))
		file2 := bytes.Join([][]byte{moov2, mdat}, nil)
		// The stco table moved with the rebuilt tree; re-locate it via the
		// sentinel that still marks its first entry, then repatch every entry.
		sentinel2 := make([]byte, 4)
		binary.BigEndian.PutUint32(sentinel2, 0xDEADBEEF)
		pb := bytes.Index(file2, sentinel2)
		if pb < 0 {
			t.Fatal("rebuilt stco table not found in ctts fixture")
		}
		for i := 0; i < n; i++ {
			binary.BigEndian.PutUint32(file2[pb+4*i:pb+4*i+4],
				uint32(len(moov2)+8+i*100))
		}
		return file2
	}
	return file
}

func indexOfBox(file []byte, typ string) int {
	for off := 0; off+8 <= len(file); {
		size := int(binary.BigEndian.Uint32(file[off : off+4]))
		if string(file[off+4:off+8]) == typ {
			return off
		}
		if size <= 0 || off+size > len(file) {
			return -1
		}
		off += size
	}
	return -1
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestScanAndIndexValidMP4(t *testing.T) {
	data := buildMP4(t, 90000, false)
	p := writeTemp(t, "ok.mp4", data)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	idx, err := mp4.IndexFile(f)
	if err != nil {
		t.Fatalf("expected valid index, got %v", err)
	}
	if idx.Video == nil || idx.Video.Codec != "avc1" {
		t.Fatalf("missing avc1 video track: %+v", idx.Video)
	}
	if got := len(idx.Video.Samples); got != 6 {
		t.Fatalf("samples = %d, want 6", got)
	}
	if !idx.Video.Samples[0].Sync || idx.Video.Samples[1].Sync {
		t.Fatal("sync sample flags not parsed from stss")
	}
	want, _ := clock.New(10, 90000)
	if !idx.Video.Samples[1].DTS.Equal(want) {
		t.Fatalf("DTS = %s, want %s", idx.Video.Samples[1].DTS, want)
	}
	kfs := idx.Video.KeyframeTimes()
	if len(kfs) != 2 {
		t.Fatalf("keyframes = %d, want 2", len(kfs))
	}
}

func TestTruncatedContainerReportsOffset(t *testing.T) {
	data := buildMP4(t, 90000, false)
	// Cut through the middle of the mdat payload.
	p := writeTemp(t, "trunc.mp4", data[:len(data)-250])
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = mp4.IndexFile(f)
	me, ok := media.As(err)
	if !ok {
		t.Fatalf("want *media.Error, got %T: %v", err, err)
	}
	if me.Kind != media.KindTruncated {
		t.Fatalf("kind = %s, want truncated_container", me.Kind)
	}
	if me.Offset < 0 {
		t.Fatalf("byte offset missing on truncation error: %v", me)
	}
}

func TestZeroTimebaseRejectedAtOffendingOffset(t *testing.T) {
	data := buildMP4(t, 0, false)
	p := writeTemp(t, "zero.mp4", data)
	f, _ := os.Open(p)
	defer f.Close()
	_, err := mp4.IndexFile(f)
	me, ok := media.As(err)
	if !ok || me.Kind != media.KindBadTimebase {
		t.Fatalf("want invalid_timebase error, got %v", err)
	}
	if me.Offset < 0 {
		t.Fatalf("zero timescale error must carry byte offset: %v", me)
	}
}

func TestPTSRollbackRejected(t *testing.T) {
	data := buildMP4(t, 90000, true)
	p := writeTemp(t, "pts.mp4", data)
	f, _ := os.Open(p)
	defer f.Close()
	_, err := mp4.IndexFile(f)
	me, ok := media.As(err)
	if !ok || me.Kind != media.KindPTSRollback {
		t.Fatalf("want pts_rollback, got %v", err)
	}
	if !me.HasTime {
		t.Fatal("PTS error must retain the media timestamp")
	}
}

func TestLengthOutOfBoundsRejected(t *testing.T) {
	data := buildMP4(t, 90000, false)
	// Corrupt the first stco offset so the sample escapes the mdat range. The
	// first entry holds the mdat payload start (immediately after moov).
	// buildMP4 always uses 6 samples of 100 bytes plus an 8-byte mdat header.
	mdatPayloadStart := len(data) - (6*100 + 8) + 8 // == len(moov)+8
	needle := make([]byte, 4)
	binary.BigEndian.PutUint32(needle, uint32(mdatPayloadStart))
	pos := bytes.Index(data, needle)
	if pos < 0 {
		t.Fatal("first stco entry not located")
	}
	binary.BigEndian.PutUint32(data[pos:pos+4], uint32(len(data)+5000))
	p := writeTemp(t, "oob.mp4", data)
	f, _ := os.Open(p)
	defer f.Close()
	_, err := mp4.IndexFile(f)
	if !media.IsKind(err, media.KindBounds) {
		t.Fatalf("want length_out_of_bounds, got %v", err)
	}
}

func TestRationalClockExactness(t *testing.T) {
	// 30000/1001 fps: ten frames must stay exact, never float-rounded.
	frame, err := clock.New(1001, 30000)
	if err != nil {
		t.Fatal(err)
	}
	ten, err := frame.Mul(10, 1)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := clock.New(1001, 3000)
	if !ten.Equal(want) {
		t.Fatalf("10 frames = %s, want %s", ten, want)
	}
	if _, err := clock.New(1, 0); err == nil {
		t.Fatal("zero denominator must be rejected")
	}
}
