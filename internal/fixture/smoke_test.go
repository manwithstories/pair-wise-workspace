package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"
	"github.com/pion/rtp"

	"mediaslice/media"
)

func TestBuildAndParseFixture(t *testing.T) {
	dir := t.TempDir()
	spec := DefaultSpec("800k", 42, media.FromSeconds(10), media.FromSeconds(2))
	plan := BuildPlan(spec)
	mp4Path := filepath.Join(dir, "v.mp4")
	rtpPath := filepath.Join(dir, "v.rtp")
	if err := WriteMP4(mp4Path, plan); err != nil {
		t.Fatal(err)
	}
	if err := WriteRTPSidecar(rtpPath, plan); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var traks, stss, elst int
	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "trak":
			traks++
		case "stss":
			stss++
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if s := box.(*mp4.Stss); s.EntryCount == 0 {
				t.Error("empty stss")
			}
		case "elst":
			elst++
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			e := box.(*mp4.Elst).Entries[0]
			if e.MediaTimeV0 != int32(spec.EditStartTicks) {
				t.Errorf("media_time=%d want %d", e.MediaTimeV0, spec.EditStartTicks)
			}
		}
		return h.Expand()
	})
	if err != nil {
		t.Fatal(err)
	}
	if traks != 2 || stss != 1 || elst != 1 {
		t.Fatalf("trak=%d stss=%d elst=%d", traks, stss, elst)
	}

	// RTP sidecar: first packets must be parseable and a wrap must occur.
	raw, err := os.ReadFile(rtpPath)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := false
	var prev uint32
	for off := 0; off < len(raw); {
		n := int(uint16(raw[off])<<8 | uint16(raw[off+1]))
		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw[off+2 : off+2+n]); err != nil {
			t.Fatal(err)
		}
		if off > 0 && pkt.Timestamp < prev {
			wrapped = true
		}
		prev = pkt.Timestamp
		off += 2 + n
	}
	if !wrapped {
		t.Error("expected at least one RTP timestamp wrap")
	}
	// 250 samples over 10s at 25fps.
	if len(plan.Video) != 250 {
		t.Fatalf("video samples=%d want 250", len(plan.Video))
	}
}
