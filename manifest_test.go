package fieldpost_test

import (
	"testing"

	"fieldpost/fixture"
	"fieldpost/manifest"
)

func TestRootManifestParsesGeneratedHLSAndDASH(t *testing.T) {
	master, err := manifest.ParseHLS("master.m3u8", fixture.HLSMaster())
	if err != nil {
		t.Fatal(err)
	}
	if len(master.Variants) != 3 {
		t.Fatalf("variants=%d", len(master.Variants))
	}
	for _, camera := range fixture.CameraNames {
		m, err := manifest.ParseHLS(camera+".m3u8", fixture.HLSCameraManifest(camera, fixture.Formats[camera]))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Segments) != fixture.SegmentsPerCamera {
			t.Fatalf("%s segments=%d", camera, len(m.Segments))
		}
	}
	d, err := manifest.ParseDASH("roving.mpd", fixture.DASHManifest())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Segments) != fixture.SegmentsPerCamera {
		t.Fatalf("dash segments=%d", len(d.Segments))
	}
}

func TestRootManifestBoundaryFixturesAreRejected(t *testing.T) {
	n := 0
	for _, bc := range fixture.BoundaryCases() {
		if bc.Kind == "box" {
			continue
		}
		var err error
		if bc.Kind == "hls" {
			_, err = manifest.ParseHLS(bc.Source, []byte(bc.HLS))
		} else {
			_, err = manifest.ParseDASH(bc.Source, []byte(bc.DASH))
		}
		if err == nil {
			t.Fatalf("%s accepted", bc.Name)
		}
		n++
	}
	if n < 4 {
		t.Fatalf("manifest boundary rejections=%d", n)
	}
}
