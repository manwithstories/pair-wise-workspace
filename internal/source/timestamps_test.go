package source

import (
	"errors"
	"testing"

	"mediapipe/internal/media"
)

func TestTimestampRollbackReportsStreamAndPosition(t *testing.T) {
	pkts := []media.Packet{
		{StreamIndex: 0, PTS: 0, DTS: 0, FileOffset: 100},
		{StreamIndex: 1, PTS: 4800, DTS: 0, FileOffset: 200},
		{StreamIndex: 0, PTS: 3003, DTS: 3003, FileOffset: 300},
		{StreamIndex: 0, PTS: 2990, DTS: 6006, FileOffset: 401},
	}
	err := ValidateTimestamps(pkts)
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeTimestampRollback {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if me.Stream != 0 || me.Position != 401 || me.ExitCode() != 2 {
		t.Fatalf("bad located error: %+v", me)
	}
}

func TestPresentationSort(t *testing.T) {
	tb := media.NewRat(1, 1000)
	pkts := []media.Packet{
		{StreamIndex: 0, PTS: 0, TimeBase: tb},
		{StreamIndex: 0, PTS: 2000, TimeBase: tb},
		{StreamIndex: 0, PTS: 1000, TimeBase: tb},
	}
	got := SortByPresentation(pkts)
	if got[0].PTS != 0 || got[1].PTS != 1000 || got[2].PTS != 2000 {
		t.Fatalf("sort failed: %+v", got)
	}
}
