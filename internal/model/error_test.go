package model

import "testing"

func TestNewErrorAndFatal(t *testing.T) {
	e := NewError(StageManifest, 2, ErrCorruptManifest, SeverityFatal, "bad json", -1, 0, false)
	if !e.Fatal() {
		t.Fatal("corrupt manifest must be fatal")
	}
	if e.Error() == "" || e.SourceOffset != -1 || e.HasPTS {
		t.Fatalf("unexpected record: %+v", e)
	}

	ptsErr := NewError(StageClock, 1, ErrPTSWrap, SeverityRecovered, "wrap", 120, 4096, true)
	if ptsErr.Fatal() {
		t.Fatal("wrap is recoverable, not fatal")
	}
	if ptsErr.Error() != "[clock/PTS_WRAP] stream=1 wrap source_offset=120 original_pts=4096" {
		t.Fatalf("unexpected render: %q", ptsErr.Error())
	}
}

func TestSortErrorsDeterministic(t *testing.T) {
	got := []Error{
		NewError(StageVerify, 0, ErrSyncDrift, SeverityError, "z", 9, 5, true),
		NewError(StageClock, 1, ErrPTSWrap, SeverityRecovered, "a", 4, 3, true),
		NewError(StageClock, 1, ErrPTSWrap, SeverityRecovered, "a", 4, 1, true),
		NewError(StageClock, 0, ErrIntegerOverflow, SeverityFatal, "b", -1, 0, false),
		NewError(StageClock, 0, ErrPTSWrap, SeverityRecovered, "c", 4, 3, true),
	}
	SortErrors(got)

	wantOrder := [][2]any{
		{StageClock, 0}, {StageClock, 0}, {StageClock, 1}, {StageClock, 1}, {StageVerify, 0},
	}
	for i, w := range wantOrder {
		if got[i].Stage != w[0] || got[i].StreamID != w[1] {
			t.Fatalf("pos %d got %s/%d want %v", i, got[i].Stage, got[i].StreamID, w)
		}
	}
	// Same stage/stream/offset: no-PTS record before PTS records.
	if got[0].HasPTS || !got[1].HasPTS {
		t.Fatal("record without PTS must sort first")
	}
	// Same stage/stream/offset/PTS code: message ordering.
	if got[2].OriginalPTS != 1 || got[3].OriginalPTS != 3 {
		t.Fatalf("pts ordering wrong: %d then %d", got[2].OriginalPTS, got[3].OriginalPTS)
	}
}

func TestSortErrorsStableAcrossShuffles(t *testing.T) {
	base := []Error{
		NewError(StageTransport, 3, ErrPacketLoss, SeverityError, "x", 10, 7, true),
		NewError(StageTransport, 1, ErrDuplicatePacket, SeverityWarning, "d", 10, 7, true),
		NewError(StageTransport, 1, ErrPacketLoss, SeverityRecovered, "r", 10, 7, true),
		NewError(StageManifest, 0, ErrCorruptManifest, SeverityFatal, "m", -1, 0, false),
	}
	SortErrors(base)
	for shuffle := 0; shuffle < 10; shuffle++ {
		cp := append([]Error(nil), base...)
		// Reverse then sort; result must match the canonical ordering.
		for i, j := 0, len(cp)-1; i < j; i, j = i+1, j-1 {
			cp[i], cp[j] = cp[j], cp[i]
		}
		SortErrors(cp)
		for i := range base {
			if cp[i] != base[i] {
				t.Fatalf("shuffle %d mismatch at %d", shuffle, i)
			}
		}
	}
}

func TestHasFatalAndCountByCode(t *testing.T) {
	errs := []Error{
		NewError(StageTransport, 1, ErrPacketLoss, SeverityRecovered, "", 1, 1, true),
		NewError(StageTransport, 1, ErrPacketLoss, SeverityError, "", 2, 2, true),
		NewError(StageTransport, 2, ErrDuplicatePacket, SeverityWarning, "", 3, 3, true),
	}
	if HasFatal(errs) {
		t.Fatal("no fatal expected")
	}
	counts := CountByCode(errs)
	if counts[ErrPacketLoss] != 2 || counts[ErrDuplicatePacket] != 1 {
		t.Fatalf("bad counts: %v", counts)
	}
	errs = append(errs, NewError(StageManifest, 0, ErrCorruptManifest, SeverityFatal, "", -1, 0, false))
	if !HasFatal(errs) {
		t.Fatal("fatal expected")
	}
}
