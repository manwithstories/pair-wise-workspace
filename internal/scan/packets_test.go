package scan

import (
	"bytes"
	"encoding/binary"
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/meta"
)

// verifyDataFraming checks the three timed-metadata framings accepted on data
// tracks: emsg boxes, MPEG-TS length-prefixed ID3 and bare ID3 tags.
func TestDataFraming(t *testing.T) {
	tag := buildID3(t, map[string]string{"TIT2": "cue", "TXXX": "value"})

	cases := []struct {
		name string
		data []byte
		kind meta.Kind
	}{
		{"bare-id3", tag, meta.KindID3},
		{"ts-length-prefixed-id3", append(be32(len(tag)), tag...), meta.KindID3},
		{"emsg-box", buildEmsgBox(t, tag), meta.KindEmsg},
	}
	for _, c := range cases {
		recs := dataRecordsBytes(c.data, Track{Index: 2, Kind: TrackData, Codec: "bin_data", TimeBase: clock.Microsecond}, 500, 0)
		if len(recs) == 0 {
			t.Fatalf("%s: no records", c.name)
		}
		if recs[0].Quarantined {
			t.Fatalf("%s: unexpectedly quarantined: %s", c.name, recs[0].Reason)
		}
		if recs[0].Kind != c.kind {
			t.Fatalf("%s: kind %s", c.name, recs[0].Kind)
		}
	}
}

func TestDataFramingCorrupt(t *testing.T) {
	// Declares 64 bytes of tag but has only 6.
	bad := append(be32(64), []byte("ID3\x04\x00")...)
	recs := dataRecordsBytes(bad, Track{Index: 2, TimeBase: clock.Microsecond}, 0, 0)
	if len(recs) != 1 || !recs[0].Quarantined {
		t.Fatalf("corrupt record not quarantined: %+v", recs)
	}
	// Oversize payload must be isolated without allocation growth.
	big := make([]byte, meta.MaxRecordBytes+10)
	recs = dataRecordsBytes(big, Track{Index: 2, TimeBase: clock.Microsecond}, 0, 0)
	if len(recs) != 1 || !recs[0].Quarantined {
		t.Fatalf("oversize record not quarantined: %+v", recs)
	}
}

func TestSEIExtraction(t *testing.T) {
	uuid := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	payload := append(append([]byte{}, uuid...), []byte("mc-sei-payload")...)
	sei := &meta.SEI{Messages: []meta.SEIMessage{{Type: 5, Payload: payload}}}
	raw, err := sei.Encode(true)
	if err != nil {
		t.Fatal(err)
	}
	// AVCC framing: 4-byte length + NAL.
	avcc := append(be32(len(raw)), raw...)
	recs := extractSEI(avcc, Track{Index: 0, Codec: "h264", TimeBase: clock.Microsecond}, 1000)
	if len(recs) != 1 || recs[0].Quarantined || recs[0].Text != "mc-sei-payload" {
		t.Fatalf("avcc extraction: %+v", recs)
	}
	// Annex B framing: start code + NAL.
	annexb := append([]byte{0, 0, 1}, raw...)
	recs = extractSEI(annexb, Track{Index: 0, Codec: "h264", TimeBase: clock.Microsecond}, 1000)
	if len(recs) != 1 || recs[0].Text != "mc-sei-payload" {
		t.Fatalf("annexb extraction: %+v", recs)
	}
	// Truncated SEI is quarantined, not dropped silently.
	truncated := append(be32(4), []byte{0x06, 0x05, 0xff, 0x10}...)
	recs = extractSEI(truncated, Track{Index: 0, Codec: "h264", TimeBase: clock.Microsecond}, 0)
	if len(recs) != 1 || !recs[0].Quarantined {
		t.Fatalf("truncated sei: %+v", recs)
	}
}

func TestConvertRejectsOverflow(t *testing.T) {
	_, err := clock.Convert(1<<62, clock.Rational{Num: 1, Den: 1}, clock.Microsecond)
	if err == nil {
		t.Fatal("expected overflow")
	}
}

func buildID3(t *testing.T, frames map[string]string) []byte {
	t.Helper()
	tag := &meta.ID3Tag{Version: 4}
	for id, v := range frames {
		body := append([]byte{3}, []byte(v)...)
		tag.Frames = append(tag.Frames, meta.ID3Frame{ID: id, Data: body})
	}
	b, err := tag.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func buildEmsgBox(t *testing.T, id3 []byte) []byte {
	t.Helper()
	e := &meta.Emsg{Version: 0, SchemeIDURI: "urn:example:emsg", Value: "v", Timescale: 1000, PTDelta: 500, EventDuration: 500, ID: 1, Message: id3}
	body, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.Write(be32(8 + len(body)))
	b.WriteString("emsg")
	b.Write(body)
	return b.Bytes()
}

func be32(n int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return b[:]
}

func TestDeriveDurationFallback(t *testing.T) {
	// A container with no duration but indexed positions must still yield a
	// usable span rather than zero.
	tl, err := clock.NewTimeline([]clock.TrackSpec{{Track: 0, TimeBase: clock.Microsecond, Start: 0}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Scan{
		Timeline:  tl,
		Keyframes: []Keyframe{{Track: 0, TimeUS: 4_000_000}},
		Meta:      []MetaRecord{{Kind: meta.KindID3, TimeUS: 2_500_000}},
	}
	if got := deriveDuration(s); got != 4_000_001 {
		t.Fatalf("derived duration %d", got)
	}
	empty := &Scan{}
	if got := deriveDuration(empty); got != 0 {
		t.Fatalf("empty scan derived %d", got)
	}
}
