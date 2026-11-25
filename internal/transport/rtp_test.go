package transport_test

import (
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/ingest"
	"mediad/internal/transport"
)

func TestRTPDuplicateAndReorderReuseTSIngestPath(t *testing.T) {
	spec := fixture.DemoRenditionSpecs()[0]
	ts, err := fixture.CleanRendition(73021, spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, arrivals, err := fixture.RTPDuplicateReorderSample(ts)
	if err != nil {
		t.Fatal(err)
	}
	dep := transport.NewDepacketizer(90000, 32)
	var recovered []byte
	for i, pkt := range raw {
		payloads, _, err := dep.PushBytes(pkt, arrivals[i])
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range payloads {
			recovered = append(recovered, p...)
		}
	}
	for _, p := range dep.Flush() {
		recovered = append(recovered, p...)
	}
	if dep.Stats.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", dep.Stats.Duplicates)
	}
	if dep.Stats.Reordered != 1 {
		t.Fatalf("reordered=%d want 1", dep.Stats.Reordered)
	}
	if len(recovered) != 10*ingest.PacketSize {
		t.Fatalf("recovered=%d bytes want 10 unique TS packets", len(recovered))
	}
	_, report, err := ingest.ScanBytes(recovered)
	if err != nil {
		t.Fatal(err)
	}
	if report.FirstBadOffset != -1 {
		t.Fatalf("recovered RTP stream has bad offset %d", report.FirstBadOffset)
	}
}
