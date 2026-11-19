package validate

import (
	"testing"

	"mediq/internal/model"
)

func TestRoundTrip(t *testing.T) {
	b := EncodeMetadata(3, 9000, model.Timebase{Num: 90000, Den: 1})
	t.Logf("meta len=%d", len(b))
	md, fault := ParseMetadata(b, 3, 0)
	if fault != nil {
		t.Fatalf("parse: %v", fault)
	}
	if md.Seq != 3 || md.Timestamp != 9000 {
		t.Fatalf("got %+v", md)
	}
}
