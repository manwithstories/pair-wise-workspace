package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"bq/internal/model"
)

func validManifest() Manifest {
	return Manifest{
		Version:   "bq/1",
		Seed:      41,
		DurationS: 60,
		MasterHz:  90000,
		Streams: []Stream{
			{ID: 0, Kind: KindVideo, Codec: "h264", Container: "mp4", File: "v0.mp4",
				Width: 64, Height: 36, TimebaseNum: 90000, TimebaseDen: 1, StartPTS: 90000,
				WrapPeriod: 1 << 20, KeyframeEvery: 10,
				EditList: []EditRange{{StartPTS: 0, EndPTS: 1000}}},
			{ID: 1, Kind: KindAudio, Codec: "aac", Container: "mp4", File: "a0.mp4",
				TimebaseNum: 48000, TimebaseDen: 1, StartPTS: 48000, KeyframeEvery: 1},
		},
	}
}

func TestParseValid(t *testing.T) {
	m := validManifest()
	data := mustJSON(t, m)
	got, errs := Parse(data)
	if len(errs) != 0 || got == nil {
		t.Fatalf("expected clean parse, got %v", errs)
	}
	if got.VideoStreamCount() != 1 {
		t.Fatal("video count wrong")
	}
	if s, ok := got.StreamByID(1); !ok || s.Codec != "aac" {
		t.Fatal("stream lookup failed")
	}
	if got.StreamKind(99) != "" {
		t.Fatal("unknown stream should be empty kind")
	}
}

func TestParseCorruptJSONReportsOffset(t *testing.T) {
	_, errs := Parse([]byte(`{"version":"bq/1",`))
	if len(errs) != 1 || errs[0].Code != model.ErrCorruptManifest || !errs[0].Fatal() {
		t.Fatalf("want fatal corrupt record, got %v", errs)
	}
	if errs[0].SourceOffset < 0 {
		t.Fatalf("expected byte offset, got %d", errs[0].SourceOffset)
	}
	if !strings.Contains(errs[0].Error(), "source_offset=") {
		t.Fatalf("error must carry offset: %s", errs[0].Error())
	}
}

func TestParseUnknownFieldRejected(t *testing.T) {
	_, errs := Parse([]byte(`{"version":"bq/1","bogus":1}`))
	if len(errs) != 1 || errs[0].Code != model.ErrCorruptManifest {
		t.Fatalf("unknown field must be rejected, got %v", errs)
	}
}

func TestParseTrailingData(t *testing.T) {
	data := append(mustJSON(t, validManifest()), []byte(" {}")...)
	_, errs := Parse(data)
	if len(errs) != 1 || errs[0].Code != model.ErrCorruptManifest {
		t.Fatalf("trailing data must be rejected, got %v", errs)
	}
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		code   string
	}{
		{"missing version", func(m *Manifest) { m.Version = "" }, model.ErrCorruptManifest},
		{"bad duration", func(m *Manifest) { m.DurationS = 0 }, model.ErrCorruptManifest},
		{"no streams", func(m *Manifest) { m.Streams = nil }, model.ErrCorruptManifest},
		{"dup stream id", func(m *Manifest) { m.Streams[1].ID = 0 }, model.ErrCorruptManifest},
		{"bad codec", func(m *Manifest) { m.Streams[1].Codec = "opus" }, model.ErrUnsupportedFormat},
		{"bad container", func(m *Manifest) { m.Streams[0].Container = "mkv" }, model.ErrUnsupportedFormat},
		{"video codec mismatch", func(m *Manifest) { m.Streams[0].Codec = "aac" }, model.ErrUnsupportedFormat},
		{"bad timebase", func(m *Manifest) { m.Streams[0].TimebaseNum = 0 }, model.ErrCorruptManifest},
		{"missing dimensions", func(m *Manifest) { m.Streams[0].Width = 0 }, model.ErrCorruptManifest},
		{"inverted edit", func(m *Manifest) { m.Streams[0].EditList[0].EndPTS = 0 }, model.ErrCorruptManifest},
		{"negative wrap", func(m *Manifest) { m.Streams[0].WrapPeriod = -1 }, model.ErrCorruptManifest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			errs := Validate(&m)
			if len(errs) != 1 || errs[0].Code != tc.code || !errs[0].Fatal() {
				t.Fatalf("want one %s fatal, got %v", tc.code, errs)
			}
		})
	}
}

func TestPacketHelpers(t *testing.T) {
	m := validManifest()
	m.Packets = []Packet{
		{StreamID: 0, Seq: 0, Kind: KindMedia, GroupID: 0, SourceOffset: 0},
		{StreamID: 0, Seq: 1, Kind: KindMedia, GroupID: 0, SourceOffset: 10},
		{StreamID: 0, Seq: -1, Kind: KindParity, GroupID: 0, SourceOffset: 20},
		{StreamID: 1, Seq: 0, Kind: KindMedia, GroupID: 1, SourceOffset: 0},
	}
	if len(m.PacketsForStream(0)) != 3 {
		t.Fatal("stream packet filter wrong")
	}
	media := MediaPackets(m.Packets)
	if len(media) != 3 {
		t.Fatal("parity not filtered")
	}
	if MaxSeq(m.Packets) != 1 {
		t.Fatal("max seq wrong")
	}
	groups := GroupPackets(m.PacketsForStream(0))
	if len(groups[0]) != 3 {
		t.Fatal("group indexing wrong")
	}
	if _, ok := FindPacket(m.Packets, 0, 1); !ok {
		t.Fatal("find packet failed")
	}
	m.Anchors = []Anchor{{StreamID: 0, AtSecond: 1}, {StreamID: 1, AtSecond: 1}}
	if len(m.AnchorsForStream(0)) != 1 {
		t.Fatal("anchor filter wrong")
	}
}

func mustJSON(t *testing.T, m Manifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
