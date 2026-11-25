package adapter

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"avpipe/internal/media"
)

func parseGenerated(t *testing.T, cfg GenConfig) *ParseResult {
	t.Helper()
	raw, err := Generate(cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	res, err := ParseContainer(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return res
}

func TestRoundTripClean(t *testing.T) {
	cfg := DefaultGenConfig(50, 7)
	res := parseGenerated(t, cfg)
	if len(res.Frames) != 50 {
		t.Fatalf("frames=%d, want 50", len(res.Frames))
	}
	for _, f := range res.Frames {
		if f.Corrupt {
			t.Fatalf("frame %d unexpectedly corrupt", f.Sequence)
		}
		if f.PTSConfidence != media.ConfidenceVerified {
			t.Fatalf("frame %d not verified: %s", f.Sequence, f.PTSConfidence)
		}
	}
	if len(res.Audio) != 50 {
		t.Fatalf("audio blocks=%d, want 50", len(res.Audio))
	}
	if res.UsedFallback {
		t.Fatal("clean stream must not use index fallback")
	}
}

func TestDeterministicPayloads(t *testing.T) {
	a, _ := Generate(DefaultGenConfig(30, 7))
	b, _ := Generate(DefaultGenConfig(30, 7))
	if !bytes.Equal(a, b) {
		t.Fatal("same seed produced different containers")
	}
}

func TestTailTruncation(t *testing.T) {
	cfg := DefaultGenConfig(40, 3)
	// First generate an intact file to learn where the records section ends.
	intact, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hdr := intact[:HeaderSize]
	audioOff := u64(hdr[20:28])
	indexOff := u64(hdr[12:20])
	recordsEnd := audioOff
	if indexOff > 0 && indexOff < recordsEnd {
		recordsEnd = indexOff
	}
	// Cut two record widths plus the footer so the record section loses its
	// last entries while still leaving a (broken) trailing section.
	cfg.TailTruncate = int(uint64(len(intact))-recordsEnd) + 2*(RecordHeaderSize+64+sha256.Size)
	res := parseGenerated(t, cfg)
	if len(res.Frames) >= 40 {
		t.Fatalf("truncated stream kept all %d records", len(res.Frames))
	}
	found := false
	for _, e := range res.Errors {
		if e.Kind == media.KindTruncated && e.Action == media.ActionTruncate {
			found = true
		}
	}
	if !found {
		t.Fatal("no truncation error reported")
	}
}

func TestChecksumCorruption(t *testing.T) {
	cfg := DefaultGenConfig(30, 5)
	cfg.CorruptEvery = 7
	res := parseGenerated(t, cfg)
	bad := 0
	for _, f := range res.Frames {
		if f.GapReason == media.KindChecksum {
			bad++
			if !f.Corrupt || f.Payload != nil {
				t.Fatalf("corrupt frame %d must be flagged with payload dropped", f.Sequence)
			}
		}
	}
	// Sequences 6,13,20,27 are corrupted for 30 frames.
	if bad != 4 {
		t.Fatalf("corrupt frames=%d, want 4", bad)
	}
}

func TestPTSRollback(t *testing.T) {
	cfg := DefaultGenConfig(20, 9)
	cfg.PTSRollback = 10
	res := parseGenerated(t, cfg)
	var sawRollback bool
	for _, e := range res.Errors {
		if e.Kind == media.KindPTSRegression {
			sawRollback = true
		}
	}
	if !sawRollback {
		t.Fatal("PTS regression not detected")
	}
}

func TestMissingIndexFallback(t *testing.T) {
	cfg := DefaultGenConfig(20, 11)
	cfg.SkipIndex = true
	res := parseGenerated(t, cfg)
	if !res.UsedFallback {
		t.Fatal("expected index fallback")
	}
	for _, f := range res.Frames {
		if f.PTSConfidence != media.ConfidenceDerived {
			t.Fatalf("frame %d not marked derived", f.Sequence)
		}
	}
	if len(res.Frames) != 20 {
		t.Fatalf("frames=%d after fallback", len(res.Frames))
	}
}

func TestUnknownVersionFailsExplicitly(t *testing.T) {
	raw, _ := Generate(DefaultGenConfig(10, 1))
	raw[4] = 99
	_, err := ParseContainer(bytes.NewReader(raw), int64(len(raw)))
	if err == nil || !strings.Contains(err.Error(), media.KindUnknownVersion) {
		t.Fatalf("want unknown_version error, got %v", err)
	}
}

func TestSizeLimitRejectsOversize(t *testing.T) {
	_, err := ParseContainer(bytes.NewReader([]byte{0}), MaxInputSize+1)
	if err == nil || !strings.Contains(err.Error(), media.KindLimitExceeded) {
		t.Fatalf("want limit error, got %v", err)
	}
}
