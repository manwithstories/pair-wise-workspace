package fault

import "testing"

func TestFaultPositionAndChain(t *testing.T) {
	f := &Fault{
		Kind:   KindTruncatedSampleTable,
		Track:  2,
		Sample: 1450,
		Offset: 4096,
		Chain:  []string{"normalize", "segment", "checksum"},
	}
	if f.FirstBlocked() != "normalize" {
		t.Fatalf("FirstBlocked = %q, want normalize", f.FirstBlocked())
	}
	want := "fault[truncated_sample_table] track=2 sample=1450 offset=4096"
	if got := f.Error(); got[:len(want)] != want {
		t.Fatalf("Error() = %q, want prefix %q", got, want)
	}
}

func TestListBlockingAndFirstBlocked(t *testing.T) {
	l := List{
		&Fault{Kind: KindTruncatedSampleTable, Recoverable: true, Chain: []string{"normalize"}},
		&Fault{Kind: KindUnsupportedLayout, Recoverable: false, Chain: []string{"segment"}},
	}
	if got := l.Blocking(); len(got) != 1 || got[0].Kind != KindUnsupportedLayout {
		t.Fatalf("Blocking() = %v", got)
	}
	if l.FirstBlockedName() != "normalize" {
		t.Fatalf("FirstBlockedName = %q", l.FirstBlockedName())
	}
	if (List{}).First() != nil {
		t.Fatal("empty list First() must be nil")
	}
}
