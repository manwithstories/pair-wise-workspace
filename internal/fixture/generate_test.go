package fixture

import (
	"os"
	"testing"
	"time"

	"mediad/internal/media"
	"mediad/internal/mp4"
)

func TestGenerateSetContainsRequiredFaultClasses(t *testing.T) {
	s, err := GenerateSet(t.TempDir(), Config{Seed: 42, Duration: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	required := []string{"backward_timestamps", "audio_rate_channels", "bad_sample_offsets", "truncated_mdat", "missing_index"}
	for _, key := range required {
		path := s.FaultFiles[key]
		if path == "" {
			t.Fatalf("missing fixture %s", key)
		}
		if st, err := os.Stat(path); err != nil || st.Size() == 0 {
			t.Fatalf("fixture %s not generated: %v", key, err)
		}
	}
	if _, err := os.Stat(s.NormalMP4); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.NormalMOV); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedNormalAndCorruptFilesAreClassifiable(t *testing.T) {
	s, err := GenerateSet(t.TempDir(), Config{Seed: 7, Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	p, _, faults, err := mp4.ReadPath(s.NormalMP4, false)
	if err != nil || len(faults) != 0 || len(p.Tracks) != 2 {
		t.Fatalf("normal read err=%v faults=%#v tracks=%d", err, faults, len(p.Tracks))
	}

	bad, _, badFaults, err := mp4.ReadPath(s.FaultFiles["bad_sample_offsets"], false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(badFaults, media.CodeBadSampleOffset) && !trackHasCode(bad, media.CodeBadSampleOffset) {
		t.Fatalf("expected bad offset fault: %#v", badFaults)
	}

	trunc, _, truncFaults, err := mp4.ReadPath(s.FaultFiles["truncated_mdat"], true)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(truncFaults, media.CodeTruncatedMDAT) {
		t.Fatalf("expected truncation fault, got %#v", truncFaults)
	}
	if len(trunc.Tracks) != 2 {
		t.Fatalf("salvaged tracks=%d", len(trunc.Tracks))
	}
}

func hasCode(fs []*media.Fault, code media.Code) bool {
	for _, f := range fs {
		if f != nil && f.Code == code {
			return true
		}
	}
	return false
}

func trackHasCode(p *media.Project, code media.Code) bool {
	for _, t := range p.Tracks {
		if hasCode(t.Faults, code) {
			return true
		}
	}
	return false
}
