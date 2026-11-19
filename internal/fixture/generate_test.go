package fixture

import (
	"testing"

	"mediamix/internal/codec"
	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate(Params{Seed: 41, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(Params{Seed: 41, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	for id, stream := range a.Streams {
		if string(b.Streams[id]) != string(stream) {
			t.Fatalf("stream %s differs between runs", id)
		}
	}
	if len(a.Sent) != len(b.Sent) {
		t.Fatal("sent packet count differs")
	}
}

func TestGenerateSeedChangesMedia(t *testing.T) {
	a, _ := Generate(Params{Seed: 1, BandwidthMBps: 3})
	b, _ := Generate(Params{Seed: 2, BandwidthMBps: 3})
	// Payload bodies are seeded, so the streams must differ between seeds.
	if string(a.Streams["cam-a"]) == string(b.Streams["cam-a"]) {
		t.Fatal("different seeds should produce different payloads")
	}
}

func TestVideoIndexPointsAtRealKeyframes(t *testing.T) {
	fx, err := Generate(Params{Seed: 41, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range fx.Tracks {
		if spec.Kind != timeline.KindVideo {
			continue
		}
		stream := fx.Streams[spec.ID]
		idx := fx.Indexes[spec.ID]
		if len(idx) == 0 {
			t.Fatalf("%s has no keyframes", spec.ID)
		}
		format, _ := codec.ParseFormat(spec.Format)
		for _, k := range idx {
			off := int(k.OffsetUs)
			if off < 0 || off+k.Size > len(stream) {
				t.Fatalf("%s keyframe out of range", spec.ID)
			}
			if !format.IsKeyframe(stream[off : off+k.Size]) {
				t.Fatalf("%s keyframe at %d is not a sync sample", spec.ID, off)
			}
		}
	}
}

func TestTruncateTrackInvalidatesIndex(t *testing.T) {
	fx, err := Generate(Params{Seed: 41, BandwidthMBps: 3, TruncateTrack: "cam-a"})
	if err != nil {
		t.Fatal(err)
	}
	stream := fx.Streams["cam-a"]
	bad := 0
	for _, k := range fx.Indexes["cam-a"] {
		if int(k.OffsetUs)+k.Size > len(stream) {
			bad++
		}
	}
	if bad == 0 {
		t.Fatal("truncation must leave at least one index entry out of range")
	}
}

func TestCorruptIndexMovesOffsetPastEnd(t *testing.T) {
	fx, err := Generate(Params{Seed: 41, BandwidthMBps: 3, CorruptIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.Corrupt) == 0 {
		t.Fatal("corrupt-index must record the defect")
	}
	for id, reason := range fx.Corrupt {
		if reason == "" {
			t.Fatalf("%s corruption has no reason", id)
		}
	}
}

func TestGenerateRejectsBadParams(t *testing.T) {
	if _, err := Generate(Params{Seed: 1, LossBurst: -1}); err == nil {
		t.Fatal("negative loss burst must be rejected")
	}
	if _, err := Generate(Params{Seed: 1, BandwidthMBps: -2}); err == nil {
		t.Fatal("negative bandwidth must be rejected")
	}
	if _, err := Generate(Params{Seed: 1, TruncateTrack: "ghost", BandwidthMBps: 1}); err == nil {
		t.Fatal("unknown truncate target must be rejected")
	}
	if _, err := Generate(Params{Seed: 1, Limits: Limits{MaxTracks: 500}}); core.CodeOf(err) != core.CodeOverLimit {
		t.Fatalf("over-limit track cap code = %s", core.CodeOf(err))
	}
}

func TestImpairDropsBurstsButKeepsOrigin(t *testing.T) {
	fx, _ := Generate(Params{Seed: 41, BandwidthMBps: 3})
	sent := fx.Sent["cam-a"]
	dels := Impair(sent, NetworkOptions{LossBurst: 7, Seed: 41})
	if dels[0].Dropped {
		t.Fatal("the first packet must never be dropped: it defines the stream origin")
	}
	dropped := 0
	for _, d := range dels {
		if d.Dropped {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("loss burst must drop packets")
	}
}

func TestSenderOnlyReturnsSentPackets(t *testing.T) {
	fx, _ := Generate(Params{Seed: 41, BandwidthMBps: 3})
	src := SenderFromDeliveries(Impair(fx.Sent["cam-a"], NetworkOptions{Seed: 41}))
	if _, ok := src.Fetch("cam-a", 0); !ok {
		t.Fatal("origin packet must be fetchable")
	}
	if _, ok := src.Fetch("cam-a", 1_000_000); ok {
		t.Fatal("cannot fetch a packet that was never sent")
	}
	if _, ok := src.Fetch("ghost", 0); ok {
		t.Fatal("cannot fetch from an unknown track")
	}
}
