package segment

import (
	"testing"

	"aircheck/internal/fixture"
	"aircheck/internal/media"
)

func cmafFixture(t *testing.T) (init []byte, seg []byte) {
	t.Helper()
	spec := fixture.DefaultClipSpec()
	init, segs := spec.BuildCMAFClip(10) // 1 s per segment
	return init, segs[0]
}

func TestDemuxCMAFBasic(t *testing.T) {
	init, seg := cmafFixture(t)
	res, err := DemuxCMAF("seg.m4s", seg, FMP4Options{Init: init, InitName: "init.mp4"})
	if err != nil {
		t.Fatalf("DemuxCMAF: %v", err)
	}
	frames, keys, samples := res.Counts()
	if frames != 30 {
		t.Fatalf("frames %d", frames)
	}
	if keys != 1 {
		t.Fatalf("keyframes %d", keys)
	}
	if samples != 46*fixture.AudioSamplesPerFrame {
		t.Fatalf("samples %d", samples)
	}
	vt, ok := res.TrackFor(media.KindVideo)
	if !ok || vt.Timescale != 90000 {
		t.Fatalf("video track %+v", vt)
	}
	at, ok := res.TrackFor(media.KindAudio)
	if !ok || at.SampleRate != 48000 {
		t.Fatalf("audio track %+v", at)
	}
	if len(res.Tracks) != 2 {
		t.Fatalf("tracks %d", len(res.Tracks))
	}
	if res.InitBytes != int64(len(init)) {
		t.Fatalf("init bytes %d", res.InitBytes)
	}
}

func TestDemuxCMAFOffsets(t *testing.T) {
	init, seg := cmafFixture(t)
	res, err := DemuxCMAF("seg.m4s", seg, FMP4Options{Init: init, InitName: "init.mp4"})
	if err != nil {
		t.Fatalf("DemuxCMAF: %v", err)
	}
	for _, p := range res.Packets {
		if p.SourceOffset < 0 || p.SourceOffset+int64(len(p.Data)) > int64(len(seg)) {
			t.Fatalf("packet extent [%d,%d) outside segment %d", p.SourceOffset, p.SourceOffset+int64(len(p.Data)), len(seg))
		}
	}
}

func TestDemuxCMAFMissingInit(t *testing.T) {
	_, seg := cmafFixture(t)
	_, err := DemuxCMAF("seg.m4s", seg, FMP4Options{})
	if err == nil {
		t.Fatal("expected missing-init fault")
	}
	if media.KindOf(err) != media.FaultMissingInit {
		t.Fatalf("kind %v", media.KindOf(err))
	}
}

func TestDemuxCMAFBadBoxLength(t *testing.T) {
	init, seg := cmafFixture(t)
	bad := append([]byte{}, seg...)
	// Find the moof box and inflate its size so it overruns its sibling.
	i := indexOfBox(bad, "moof")
	if i < 0 {
		t.Fatal("no moof")
	}
	// rewrite 32-bit size to something far larger than the segment
	putUint32(bad[i:i+4], uint32(len(bad)*2))
	_, err := DemuxCMAF("seg.m4s", bad, FMP4Options{Init: init, InitName: "init.mp4"})
	if err == nil {
		t.Fatal("expected bad-box-length fault")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Kind != media.FaultBadBoxLength {
		t.Fatalf("fault %v", err)
	}
	if f.Pos.Offset != int64(i) {
		t.Fatalf("fault offset %d want %d", f.Pos.Offset, i)
	}
}

func TestDemuxCMAFTruncatedSampleData(t *testing.T) {
	init, seg := cmafFixture(t)
	// Cut mid-mdat but also shrink mdat's declared size so the container
	// frame stays consistent and only the sample data is short.
	cut := append([]byte{}, seg[:len(seg)-200]...)
	i := indexOfBox(cut, "mdat")
	if i < 0 {
		t.Fatal("no mdat")
	}
	putUint32(cut[i:i+4], uint32(len(cut)-i))
	_, err := DemuxCMAF("seg.m4s", cut, FMP4Options{Init: init, InitName: "init.mp4"})
	if err == nil {
		t.Fatal("expected truncation fault")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Kind != media.FaultTruncated {
		t.Fatalf("fault %v", err)
	}
	if f.Pos.Offset < 0 {
		t.Fatalf("missing offset: %+v", f.Pos)
	}
}

func TestPayloadHashAcrossFormats(t *testing.T) {
	spec := fixture.DefaultClipSpec()
	tsData := spec.BuildTS(0, 30, 0, 46)
	init, segs := spec.BuildCMAFClip(10)
	tsRes, err := DemuxTS("s.ts", tsData, TSOptions{})
	if err != nil {
		t.Fatalf("ts: %v", err)
	}
	cmRes, err := DemuxCMAF("s.m4s", segs[0], FMP4Options{Init: init, InitName: "init.mp4"})
	if err != nil {
		t.Fatalf("cmaf: %v", err)
	}
	// The payload bytes are generated from the same (kind,index,length) seed,
	// so a packet-identical pair must hash identically across containers.
	if tsRes.PayloadHashString() != cmRes.PayloadHashString() {
		t.Fatalf("cross-container hash mismatch:\n%s\n%s", tsRes.PayloadHashString(), cmRes.PayloadHashString())
	}
}

func indexOfBox(b []byte, typ string) int {
	for i := 0; i+8 <= len(b); i += 4 {
		if string(b[i+4:i+8]) == typ {
			return i
		}
	}
	return -1
}

func putUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
