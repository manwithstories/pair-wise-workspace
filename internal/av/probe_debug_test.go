package av

import (
	"path/filepath"
	"testing"

	"mediad/internal/fixtures"
)

// TestDemuxFlagsTruncatedPacketRanges builds the truncated fixture and asserts
// the final audio packet is surfaced as corrupt with its track and time, which
// is what lets the pipeline skip exactly the damaged interval.
func TestDemuxFlagsTruncatedPacketRanges(t *testing.T) {
	root := t.TempDir()
	if _, err := fixtures.Build(fixtures.Spec{Root: root, Seed: 41, Case: fixtures.CaseTruncated}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "media", "camBroken.mov")
	d, err := NewDemuxer(path)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := d.ProbeStreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) == 0 {
		t.Fatal("no audio streams")
	}
	audio := infos[0]
	packets, err := d.ReadAll(audio.StreamIndex)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(packets) == 0 {
		t.Fatal("no packets")
	}
	last := packets[len(packets)-1]
	if !last.Corrupt {
		t.Fatalf("final packet not flagged corrupt: pts=%d dur=%d size=%d", last.PTS, last.Duration, len(last.Data))
	}
	if last.StreamIndex != audio.StreamIndex {
		t.Fatalf("damage attributed to stream %d, want %d", last.StreamIndex, audio.StreamIndex)
	}
	if last.PTS <= 0 {
		t.Fatalf("damage time not recorded: %d", last.PTS)
	}
}
