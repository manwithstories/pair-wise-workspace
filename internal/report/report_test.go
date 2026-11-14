package report

import (
	"bytes"
	"encoding/json"
	"testing"

	"tsaudit/internal/psi"
)

func TestJSONFieldNames(t *testing.T) {
	rep := &Report{
		PATVersionChanges: 5, PMTVersionChanges: map[uint16]int{1: 4},
		PIDMap:         []psi.PIDEntry{{PID: 0x200, Program: 1, Role: "ES", PacketCount: 123}},
		ESPacketsTotal: 123, CCJumps: 1, CCDuplicates: 2, LostPackets: 3, Segments: 4,
	}
	var buf bytes.Buffer
	if err := rep.RenderJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pat_version_changes", "pmt_version_changes", "pid_map",
		"es_packets_total", "cc_jumps", "cc_duplicates", "lost_packets", "segments"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing JSON field %q", key)
		}
	}
}
