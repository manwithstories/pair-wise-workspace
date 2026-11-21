package fixture

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"
)

func TestBuildParses(t *testing.T) {
	p := DefaultPlan(3, 4)
	b := p.Build()
	if len(b) < 1000 {
		t.Fatalf("suspiciously small fixture: %d", len(b))
	}
	n := 0
	_, err := mp4.ReadBoxStructure(bytes.NewReader(b), func(h *mp4.ReadHandle) (interface{}, error) {
		n++
		if h.BoxInfo.Type == mp4.BoxTypeMdat() {
			return nil, nil
		}
		if h.BoxInfo.Type == mp4.BoxTypeStsd() {
			_, _, err := h.ReadPayload()
			return nil, err
		}
		if h.BoxInfo.IsSupportedType() {
			_, err := h.Expand()
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n < 30 {
		t.Fatalf("only %d boxes", n)
	}
}
