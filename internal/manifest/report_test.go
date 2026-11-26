package manifest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBitrateAndAlignment(t *testing.T) {
	m := New("job", 17, "sha")
	// 30 seconds, about 4.5 MB => 1200 kbps.
	m.SetBitrate(4_500_000, 30, 1200)
	if m.Stats.ActualKbps != 1200 || m.Stats.DeviationPct != 0 {
		t.Fatalf("bitrate stats = %+v", m.Stats)
	}
	m.AddSegment(Segment{AlignmentErrorMS: 12.5})
	m.AddSegment(Segment{AlignmentErrorMS: 41.9})
	if m.Stats.ManifestErrorMS != 41.9 {
		t.Fatalf("alignment = %v", m.Stats.ManifestErrorMS)
	}
	if err := m.ValidateAlignment(42); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLineFlattensRequiredRecoveryFields(t *testing.T) {
	var buf bytes.Buffer
	r := Recovery{DuplicatePackets: 3, ReorderedPackets: 4, RecoveredPackets: 2,
		CacheHit: true, SourceOK: true, SHA256OK: true}
	if err := JSONLine(&buf, "recovery", r); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", line, err)
	}
	for _, key := range []string{"event", "duplicate_packets", "reordered_packets", "recovered_packets", "cache_hit", "source_ok", "sha256_ok"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in %s", key, line)
		}
	}
}

func TestFaultLineShape(t *testing.T) {
	var buf bytes.Buffer
	_ = JSONLine(&buf, "fault", FaultLine{Case: "truncated_moov", ErrorCode: "truncated_moov", ExitCode: 2, OK: true})
	if !strings.Contains(buf.String(), `"case":"truncated_moov"`) || !strings.Contains(buf.String(), `"error_code":"truncated_moov"`) {
		t.Fatal(buf.String())
	}
}
