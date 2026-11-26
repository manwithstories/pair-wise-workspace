// Package cgosmoke verifies that go-astiav compiles and links against the
// system FFmpeg. It is not part of the mediaflow data plane.
package cgosmoke

import (
	"testing"

	"github.com/asticode/go-astiav"
)

func TestCGOLinkAndRational(t *testing.T) {
	astiav.SetLogLevel(astiav.LogLevelQuiet)
	r := astiav.NewRational(1, 90000)
	if r.Num() != 1 || r.Den() != 90000 {
		t.Fatalf("unexpected rational: %d/%d", r.Num(), r.Den())
	}
	pkt := astiav.AllocPacket()
	if pkt == nil {
		t.Fatal("AllocPacket returned nil")
	}
	pkt.Free()
}
