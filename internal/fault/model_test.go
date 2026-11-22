package fault

import (
	"strings"
	"testing"
)

func TestFaultRenderingIncludesLocation(t *testing.T) {
	f := New(ModuleEBML, KindTruncated, NewOffset(0x20), "element extends past EOF").OnTrack(1)
	got := f.Error()
	for _, want := range []string{"ebml", "truncated-element", "0x20", "track=1", "extends past EOF"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error()=%q missing %q", got, want)
		}
	}
	if New(ModuleEBML, KindBadElement, NoOffset(), "x").Error() == "" {
		t.Fatal("empty rendering")
	}
}

func TestSetOrderingAndExitCodes(t *testing.T) {
	var s Set
	s.Add(New(ModuleTimeline, KindTimestampRegress, NewOffset(30), "regress"))
	s.Add(New(ModuleTransport, KindPacketGap, NoOffset(), "gap"))
	s.Add(New(ModuleEBML, KindTruncated, NewOffset(4), "trunc"))

	if got := s.Faults()[0].Offset.Value; got != 4 {
		t.Fatalf("first offset=%d want 4", got)
	}
	if s.ExitCode() != ExitDamaged {
		t.Fatalf("exit=%d want %d", s.ExitCode(), ExitDamaged)
	}
	if s.Count(KindPacketGap) != 1 {
		t.Fatal("count")
	}
	if len(s.Kinds()) != 3 {
		t.Fatalf("kinds=%v", s.Kinds())
	}
	if (*Set)(nil).ExitCode() != ExitOK || (*Set)(nil).Err() != nil {
		t.Fatal("nil set")
	}
}

func TestFatalAndVerificationExit(t *testing.T) {
	var s Set
	s.Add(New(ModuleManifest, KindVerification, NewOffset(9), "too far").Fatal())
	if !s.HasFatal() || s.ExitCode() != ExitIntegrity {
		t.Fatalf("fatal exit=%d", s.ExitCode())
	}
	var v Set
	v.Add(New(ModuleManifest, KindVerification, NewOffset(9), "too far"))
	if v.ExitCode() != ExitIntegrity {
		t.Fatalf("verification exit=%d", v.ExitCode())
	}
	if err := v.Err(); err == nil || !strings.Contains(err.Error(), "too far") {
		t.Fatalf("err=%v", err)
	}
}

func TestRecoveryEventsDoNotFailAcceptance(t *testing.T) {
	var s Set
	s.Add(New(ModuleJournal, KindJournalTail, NoOffset(), "torn tail").AsRecovery())
	s.Add(New(ModuleTxn, KindInterrupted, NoOffset(), "rolled back repair").AsRecovery())
	if s.ExitCode() != ExitOK {
		t.Fatalf("recovery-only exit=%d want %d", s.ExitCode(), ExitOK)
	}
	// Recovery events plus real damage still surface the damage.
	s.Add(New(ModuleEBML, KindTruncated, NewOffset(3), "cut"))
	if s.ExitCode() != ExitDamaged {
		t.Fatalf("mixed exit=%d", s.ExitCode())
	}
}

func TestOffsetUnknownVersusZero(t *testing.T) {
	if NewOffset(0).String() != "0x0" {
		t.Fatal("zero offset must render")
	}
	if NoOffset().String() != "?" {
		t.Fatal("unknown offset must render as ?")
	}
}
