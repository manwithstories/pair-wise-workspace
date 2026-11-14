package report

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"

	"tsprobe/internal/demux"
	"tsprobe/internal/fixture"
	"tsprobe/internal/psi"
	"tsprobe/internal/slicer"
	"tsprobe/internal/tspacket"
)

func TestBuildAndWriteStableJSON(t *testing.T) {
	var stream bytes.Buffer
	if _, err := fixture.Generate(&stream, fixture.Options{
		Programs: 2, Duration: 12 * time.Second, Loss: 7, CCJumps: 3, Versions: 4,
	}); err != nil {
		t.Fatal(err)
	}
	reader := tspacket.NewReader(bytes.NewReader(stream.Bytes()), 8)
	store := psi.NewStore()
	dmx := demux.New(store)
	for {
		packet, err := reader.ReadPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dmx.Process(packet); err != nil {
			t.Fatal(err)
		}
	}
	slices := []slicer.SliceInfo{{File: "clip_000.ts", PacketOffset: 3, PacketCount: 10, ByteLength: 1880,
		PATVersion: 1, PMTVersions: []slicer.PMTEntry{{Program: 1, Version: 1}}}}
	summary := Build(store, reader.Stats(), dmx.Stats(), slices, 0)
	var encoded bytes.Buffer
	if err := summary.WriteJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"pat_versions", "pmt_versions", "pid_map", "packet_counts", "cc_jumps",
		"lost_packets", "crc_errors", "slices", "mismatches",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("JSON missing %q: %s", key, encoded.String())
		}
	}
	if summary.PATVersions != 4 || summary.CCJumps != 3 || summary.LostPackets != 7 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.PacketCounts[0x100] != 1180 || summary.PacketCounts[0x1000] != 24 {
		t.Fatalf("packet counts=%v", summary.PacketCounts)
	}
	var text bytes.Buffer
	if err := summary.WriteText(&text, store, reader.Stats()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(text.Bytes(), []byte("Dangling programs: [2]")) ||
		!bytes.Contains(text.Bytes(), []byte("PAT version rollbacks: 1")) {
		t.Fatalf("text diagnostics missing:\n%s", text.String())
	}
}
