package fixture

import (
	"bytes"
	"testing"

	"archivecheck.local/archivecheck/internal/demux"
)

// The unknown-size fixture must be structurally honest: no Segment size, no
// Cluster sizes, yet still fully parseable. These tests pin that shape so a
// future fixture edit cannot quietly turn it into a known-size file.

func TestUnknownSizeFixtureShape(t *testing.T) {
	fx, err := New(DamageUnknownSize, 17)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(fx.Bytes, UnknownSize) < 2 {
		t.Fatal("expected unknown-size markers for Segment and Clusters")
	}
	var res demux.Result
	opts := demux.DefaultOptions()
	if err := demux.ScanReader(bytes.NewReader(fx.Bytes), opts, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Info.SegmentUnknown {
		t.Fatal("segment not reported as unknown-size")
	}
	if res.Info.SegmentEnd != int64(len(fx.Bytes)) {
		t.Fatalf("segment end=%d input=%d", res.Info.SegmentEnd, len(fx.Bytes))
	}
	// Every cluster must have been discovered despite the missing sizes.
	if len(res.ClusterOffsets) < 2 {
		t.Fatalf("clusters=%d", len(res.ClusterOffsets))
	}
}

func TestUnknownSizeFixtureHasNoClusterSizeBytes(t *testing.T) {
	fx, err := New(DamageUnknownSize, 17)
	if err != nil {
		t.Fatal(err)
	}
	clusterID := EncID(0x1F43B675)
	seen := 0
	for i := 0; i+len(clusterID) < len(fx.Bytes); i++ {
		if bytes.Equal(fx.Bytes[i:i+len(clusterID)], clusterID) {
			seen++
			if fx.Bytes[i+len(clusterID)] != 0xFF {
				t.Fatalf("cluster at %d declares a size instead of unknown", i)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no clusters found")
	}
}
