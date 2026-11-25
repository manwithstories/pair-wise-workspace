package codec_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/codec"
	"mediad/internal/fault"
	"mediad/internal/manifest"
)

func validDescriptor() manifest.CodecDescriptor {
	return manifest.CodecDescriptor{
		Container: "fmp4", VideoCodec: "avc1", AudioCodec: "pcm_s16le",
		AdapterVersion: codec.AdapterVersion, SpecVersion: codec.SupportedSpecVersion,
	}
}

func TestAdapterV1RejectsUnknownVersionAndCodec(t *testing.T) {
	a := codec.NewV1()
	badVersion := validDescriptor()
	badVersion.SpecVersion = "future-v2"
	_, err := a.Open(badVersion)
	if f, ok := fault.As(err); !ok || f.Code != fault.UnknownVersion {
		t.Fatalf("version err=%v want E_UNKNOWN_VERSION", err)
	}

	badCodec := validDescriptor()
	badCodec.VideoCodec = "vp9"
	_, err = a.Open(badCodec)
	if f, ok := fault.As(err); !ok || f.Code != fault.UnsupportedCodec {
		t.Fatalf("codec err=%v want E_UNSUPPORTED_CODEC", err)
	}

	badContainer := validDescriptor()
	badContainer.Container = "mpegts"
	_, err = a.Open(badContainer)
	if f, ok := fault.As(err); !ok || f.Code != fault.UnsupportedCodec {
		t.Fatalf("container err=%v want E_UNSUPPORTED_CODEC", err)
	}
}

func TestPCM16LERoundtrip12Blocks(t *testing.T) {
	s, err := codec.NewV1().Open(validDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	for block := 0; block < 12; block++ {
		in := make([]int16, 256)
		for i := range in {
			in[i] = int16((block*4096 + i*13) & 0xffff)
		}
		ok, err := s.RoundtripBlock(in)
		if err != nil || !ok {
			t.Fatalf("block %d ok=%v err=%v", block, ok, err)
		}
	}
}

func buildAVCCFrames(total int) []byte {
	var data []byte
	for frame := 0; frame < total; frame++ {
		header := byte(0x41)
		if frame%3 == 0 {
			header = 0x65
		}
		nal := append([]byte{header}, bytes.Repeat([]byte{byte(frame + 1)}, 24)...)
		n := make([]byte, 4)
		n[0] = byte(len(nal) >> 24)
		n[1] = byte(len(nal) >> 16)
		n[2] = byte(len(nal) >> 8)
		n[3] = byte(len(nal))
		data = append(data, n...)
		data = append(data, nal...)
	}
	return data
}

func TestH264KeyframeScanAndIndexValidation(t *testing.T) {
	const total = 144
	data := buildAVCCFrames(total)
	nals, err := codec.ScanAVCC("frames", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nals) != total {
		t.Fatalf("NALs=%d want %d", len(nals), total)
	}
	actual := codec.KeyframeIndices(nals)
	var expected []int
	for i := 0; i < total; i++ {
		if i%3 == 0 {
			expected = append(expected, i)
		}
	}
	hits, err := codec.ValidateKeyframeIndex("frames", actual, expected)
	if err != nil {
		t.Fatalf("ValidateKeyframeIndex: %v", err)
	}
	matched, n, rate := codec.KeyframeScore(hits)
	if n != 48 || matched != 48 || rate != 1 {
		t.Fatalf("score=%d/%d %.3f", matched, n, rate)
	}

	annexB := []byte{0, 0, 0, 1, 0x65, 0xaa, 0xbb}
	an, err := codec.ScanAnnexB("annexb", annexB)
	if err != nil || len(an) != 1 || !an[0].IDR {
		t.Fatalf("annexb nals=%+v err=%v", an, err)
	}
}

func TestRTPWraparoundPreservesMediaTime(t *testing.T) {
	const rate = 90000
	m := codec.NewRTPMapper(rate, 65530, math.MaxUint32-3000)
	for frame := 0; frame < 24; frame++ {
		seq := uint16(65530 + frame)
		rawTS := int64(math.MaxUint32-3000) + int64(frame)*3000
		ts := uint32(rawTS & 0xffffffff)
		mp, err := m.Map(seq, ts, frame%3 == 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := clock.Must(int64(frame)*3000, rate)
		if clock.Cmp(mp.MediaTime, want) != 0 {
			t.Fatalf("frame %d time=%s want %s", frame, mp.MediaTime, want)
		}
	}

	var ext uint64 = 100
	for i := 1; i < 70000; i++ {
		ext = codec.ExtendSequence(ext, uint16(100+i))
	}
	if ext != 70099 {
		t.Fatalf("extended sequence=%d want 70099", ext)
	}

	pkt := rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, Timestamp: 2}, Payload: []byte("x")}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var decoded rtp.Packet
	if err := decoded.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if decoded.Timestamp != 2 || string(decoded.Payload) != "x" {
		t.Fatalf("pion packet roundtrip failed: %+v", decoded)
	}
}
