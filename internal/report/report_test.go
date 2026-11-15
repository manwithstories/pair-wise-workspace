package report

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"pairwise.local/relayctl/internal/relay"
	"pairwise.local/relayctl/internal/signal"
)

func sampleReport() Report {
	return Build(relay.Stats{
		CandidatePairs: 4, ForwardedPackets: 1786, DroppedPackets: 14, LatePackets: 9,
		JitterMS: relay.JitterStats{P50: 0.9, P95: 4.3, Max: 8.7, Samples: 1800},
		Egresses: []relay.EgressStats{{Endpoint: "s-2", SSRC: 7, Forwarded: 600,
			Dropped: 5, Late: 3, JitterMS: relay.JitterStats{P50: 0.8, P95: 4.1, Max: 8.0, Samples: 600}}},
	}, signal.Stats{Rejected: 2})
}

func TestJSONContractRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out", "report.json")
	if err := WriteFile(path, sampleReport()); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := WriteLine(&encoded, loaded); err != nil {
		t.Fatal(err)
	}
	if strings.Count(encoded.String(), "\n") != 1 {
		t.Fatalf("stdout JSON must be one line: %q", encoded.String())
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"candidate_pairs", "forwarded_packets", "dropped_packets", "late_packets", "jitter_ms", "egress_failed"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing JSON field %s", key)
		}
	}
}

func TestTable(t *testing.T) {
	var output bytes.Buffer
	if err := WriteTable(&output, sampleReport()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "s-2") || !strings.Contains(output.String(), "p95=4.300") {
		t.Fatalf("unexpected table: %s", output.String())
	}
}
