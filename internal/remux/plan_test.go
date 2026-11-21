package remux

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"mkvarchive/internal/model"
	"mkvarchive/internal/track"
)

func TestNewPlanFreezesWarnings(t *testing.T) {
	w := model.NewWarning("COLOR_METADATA_MISSING", "no colour", 11)
	mappings := []track.Mapping{{SourceUID: 1, SourceNumber: 1, ArchiveID: 1, Kind: model.KindVideo}}
	p := NewPlan(1_000_000, mappings, []*model.ArchiveError{w})
	if p.Version != PlanVersion || p.TimestampScale != 1_000_000 || len(p.Warnings) != 1 {
		t.Fatalf("plan fields wrong: %+v", p)
	}
	ordered := p.OrderedMappings()
	ordered[0].ArchiveID = 99
	if p.Mappings[0].ArchiveID != 1 {
		t.Fatal("OrderedMappings must return a defensive copy")
	}
}

func TestDigestBlockDeterministic(t *testing.T) {
	m := track.Mapping{SourceUID: 7, SourceNumber: 3, ArchiveID: 2, Kind: model.KindAudio}
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	d1 := DigestBlock(m, 1234, 567, payload)
	d2 := DigestBlock(m, 1234, 567, payload)
	if d1 != d2 {
		t.Fatal("digest must be deterministic for identical input")
	}
	want := sha256.Sum256(payload)
	if d1.SHA256 != hex.EncodeToString(want[:]) || d1.Bytes != 4 {
		t.Fatalf("digest wrong: %+v", d1)
	}
	if d1.ArchiveTrackID != 2 || d1.SourceOffset != 1234 || d1.RawTimecode != 567 {
		t.Fatalf("position fields wrong: %+v", d1)
	}
}
