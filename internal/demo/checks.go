package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"fieldpost/demux"
	"fieldpost/fixture"
	"fieldpost/internal/core"
	"fieldpost/manifest"
)

func runBoundaries() (int, error) {
	rejected := 0
	for _, bc := range fixture.BoundaryCases() {
		var err error
		switch bc.Kind {
		case "hls":
			_, err = manifest.ParseHLS(bc.Source, []byte(bc.HLS))
		case "dash":
			_, err = manifest.ParseDASH(bc.Source, []byte(bc.DASH))
		case "box":
			_, err = demux.ParseMP4("boundary", bc.Source, bc.BoxBytes)
		default:
			return rejected, core.UsageError("unknown boundary kind %q", bc.Kind)
		}
		if err == nil {
			return rejected, core.InvariantError(core.StageInvariant, bc.Source, 0,
				"boundary case %q was accepted", bc.Name)
		}
		if got := core.ExitCode(err); got != bc.ExpectExit {
			return rejected, core.InvariantError(core.StageInvariant, bc.Source, 0,
				"boundary %q exit=%d want %d err=%v", bc.Name, got, bc.ExpectExit, err)
		}
		rejected++
	}
	return rejected, nil
}

func canonicalSignature(seed int64) string {
	h := sha256.New()
	for ci, camera := range fixture.CameraNames {
		for i := 0; i < fixture.SegmentsPerCamera; i++ {
			seg := fixture.MediaSegment(seed, camera, i)
			fmt.Fprintf(h, "%s|%d|%s\n", camera, i, core.EssenceHash(seg))
			fmt.Fprintf(h, "index=%d keyframes=%d\n", ci, countKeyFlags(seg))
		}
	}
	for _, bc := range fixture.BoundaryCases() {
		fmt.Fprintf(h, "boundary=%s:%d\n", bc.Kind, bc.ExpectExit)
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:])
}

func countKeyFlags(seg *core.Segment) int {
	video := map[uint32]bool{}
	for _, t := range seg.Tracks {
		if t.Kind == core.TrackVideo {
			video[t.ID] = true
		}
	}
	n := 0
	for _, sm := range seg.Samples {
		if video[sm.TrackID] && sm.Flags&core.FlagSync != 0 {
			n++
		}
	}
	return n
}

func validateManifests() error {
	if _, err := manifest.ParseHLS("master.m3u8", fixture.HLSMaster()); err != nil {
		return err
	}
	for _, camera := range fixture.CameraNames {
		m, err := manifest.ParseHLS(camera+".m3u8", fixture.HLSCameraManifest(camera, fixture.Formats[camera]))
		if err != nil {
			return err
		}
		if len(m.Segments) != fixture.SegmentsPerCamera {
			return core.ManifestError(camera+".m3u8", 0, "segment count=%d", len(m.Segments))
		}
	}
	d, err := manifest.ParseDASH("roving.mpd", fixture.DASHManifest())
	if err != nil {
		return err
	}
	if len(d.DASH.Representations) < 2 {
		return core.ManifestError("roving.mpd", 0, "expected audio/video representations")
	}
	return nil
}
