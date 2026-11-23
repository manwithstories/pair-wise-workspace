package remux

import (
	"bytes"
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/rtpnet"
	"mediacore/internal/sample"
)

func orderedSamples() []*sample.Sample {
	codecs := []sample.Codec{sample.CodecH264, sample.CodecAAC, sample.CodecOpus}
	var out []*sample.Sample
	for track := uint32(1); track <= 3; track++ {
		for i := 0; i < 8; i++ {
			out = append(out, &sample.Sample{
				TrackIndex: int(track - 1), TrackID: track, Seq: i,
				Codec: codecs[track-1], PTS: clock.Tick(i) * 3000,
				Duration: 3000, Data: []byte{byte(track), byte(i), 0xAB, 0xCD},
			})
		}
	}
	return out
}

func profiles() []TrackProfile {
	return []TrackProfile{
		{TrackID: 1, Codec: sample.CodecH264},
		{TrackID: 2, Codec: sample.CodecAAC},
		{TrackID: 3, Codec: sample.CodecOpus},
	}
}

func TestWriteContainerDeterministic(t *testing.T) {
	in := orderedSamples()
	var a, b bytes.Buffer
	ma, err := WriteContainer(&a, in, profiles())
	if err != nil {
		t.Fatal(err)
	}
	mb, err := WriteContainer(&b, in, profiles())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("same input must produce byte-identical output")
	}
	if ma.SHA256 != mb.SHA256 || ma.SHA256 == "" {
		t.Fatal("rerun checksum mismatch or empty")
	}
	if ma.Samples != 24 || ma.Tracks != 3 || ma.RTPpackets < 24 {
		t.Fatalf("unexpected metrics: %+v", ma)
	}
}

func TestChecksumCoversRTP(t *testing.T) {
	in := orderedSamples()
	var a, b bytes.Buffer
	ma, _ := WriteContainer(&a, in, profiles())
	// Mutate one sample byte; checksum must change.
	in2 := orderedSamples()
	in2[0].Data[0] ^= 0xFF
	mb, _ := WriteContainer(&b, in2, profiles())
	if ma.SHA256 == mb.SHA256 {
		t.Fatal("checksum must change when compressed payload changes")
	}
}

func TestRTPRoundTrip(t *testing.T) {
	s := &sample.Sample{TrackID: 1, PTS: 90000, Data: bytes.Repeat([]byte{0x55}, 3000)}
	pz := rtpnet.NewPacketizer(rtpnet.TrackConfig{SSRC: 0x1000, PayloadType: 96,
		ClockRate: clock.Rate}, rtpnet.DefaultMTU)
	pkts, err := pz.Packetize(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) < 2 {
		t.Fatalf("large sample must fragment, got %d packets", len(pkts))
	}
	// Only the last fragment carries the marker bit.
	for i, p := range pkts {
		raw, err := rtpnet.Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := rtpnet.Decode(raw)
		if err != nil {
			t.Fatalf("pion failed to parse our packet: %v", err)
		}
		wantMarker := i == len(pkts)-1
		if dec.Marker != wantMarker {
			t.Fatalf("fragment %d marker=%v want %v", i, dec.Marker, wantMarker)
		}
		if dec.Timestamp != 90000 || dec.SSRC != 0x1000 || dec.PayloadType != 96 {
			t.Fatalf("RTP header fields wrong: %+v", dec.Header)
		}
	}
}

func TestOutputFramesSelfDescribing(t *testing.T) {
	in := orderedSamples()
	var buf bytes.Buffer
	if _, err := WriteContainer(&buf, in, profiles()); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	// Skip ftyp (first box) then every record must parse as a framed packet.
	ftypSize := int(raw[0])<<24 | int(raw[1])<<16 | int(raw[2])<<8 | int(raw[3])
	pos := ftypSize
	count := 0
	for pos < len(raw) {
		trackID, payload, bad, ok := sample.ParseFrame(raw[pos:])
		if !ok {
			t.Fatalf("output frame at %d invalid at +%d", pos, bad)
		}
		if trackID < 1 || trackID > 3 {
			t.Fatalf("unexpected track id %d", trackID)
		}
		if _, err := rtpnet.Decode(payload); err != nil {
			t.Fatalf("framed payload not valid RTP: %v", err)
		}
		pos += sample.FrameHeaderLen + len(payload)
		count++
	}
	if count == 0 {
		t.Fatal("no frames written")
	}
}
