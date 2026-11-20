package timeline

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/pion/rtp"
)

func frame(t *testing.T, track string, seq, ts uint64, ok bool) *ingest.Frame {
	t.Helper()
	p := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(seq), Timestamp: uint32(ts)}, Payload: []byte{byte(seq), 1, 2}}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	if !ok {
		sha = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return &ingest.Frame{
		TrackID: track, Seq: seq, DeviceTS: ts, Rate: 90000,
		Payload: b, SourceSHA: sha, ChecksumOK: ok, Offset: int64(seq * 100),
	}
}

func videoClock() model.Clock {
	return model.Clock{TrackID: "v1", Rate: 90000, AnchorSeq: 0, AnchorDeviceTS: 0, AnchorMasterTS: 0, FrameStep: 3000}
}

func TestRepairNormalAndDrift(t *testing.T) {
	frames := []*ingest.Frame{
		frame(t, "v1", 0, 0, true),
		frame(t, "v1", 1, 3000, true),
		frame(t, "v1", 2, 6005, true), // 5 ticks drift, within one frame
	}
	rep := Repair(frames, videoClock(), "in.jsonl")
	if model.HasFatal(rep.Faults) {
		t.Fatalf("unexpected fatal: %v", rep.Faults)
	}
	if rep.Frames[0].MasterTS != 0 || rep.Frames[1].MasterTS != 3000 {
		t.Fatalf("anchor/normal wrong: %d %d", rep.Frames[0].MasterTS, rep.Frames[1].MasterTS)
	}
	// Small drift is kept, not corrected.
	if rep.Frames[2].MasterTS != 6005 || rep.Frames[2].Inferred {
		t.Fatalf("drift frame: ts=%d inferred=%v", rep.Frames[2].MasterTS, rep.Frames[2].Inferred)
	}
}

func TestRepairRollover(t *testing.T) {
	near := uint64(model.RTPMod - 6000)
	frames := []*ingest.Frame{
		frame(t, "v1", 0, model.RTPMod-6000, true), // anchor near the modulus
		frame(t, "v1", 1, model.RTPMod-3000, true),
		frame(t, "v1", 2, 0, true), // rolled over; expectation is 2*3000=6000
	}
	clock := videoClock()
	clock.AnchorDeviceTS = near
	rep := Repair(frames, clock, "in.jsonl")
	if rep.Rollovers != 1 {
		t.Fatalf("rollovers=%d want 1: %v", rep.Rollovers, rep.Faults)
	}
	rf := rep.Frames[2]
	if !rf.RolloverSeen {
		t.Fatal("rollover flag missing")
	}
	// Unwrapped conversion lands exactly on the sequence expectation.
	if rf.MasterTS != 6000 {
		t.Fatalf("post-rollover master ts=%d want 6000", rf.MasterTS)
	}
}

func TestRepairBackwardsAndMissing(t *testing.T) {
	frames := []*ingest.Frame{
		frame(t, "v1", 0, 0, true),
		frame(t, "v1", 1, 3000, true),
		frame(t, "v1", 2, 500, true),   // backwards without rollover
		frame(t, "v1", 4, 12000, true), // seq 3 missing
	}
	rep := Repair(frames, videoClock(), "in.jsonl")
	if rep.Backwards != 1 || rep.Missing != 1 {
		t.Fatalf("backwards=%d missing=%d faults=%v", rep.Backwards, rep.Missing, rep.Faults)
	}
	if !rep.Frames[2].Inferred || rep.Frames[2].MasterTS != 6000 {
		t.Fatalf("backwards frame not corrected: %+v", rep.Frames[2])
	}
	// Source checksum preserved on every repaired frame.
	for _, rf := range rep.Frames {
		if rf.Frame.SourceSHA == "" {
			t.Fatal("source checksum lost")
		}
	}
}

func TestRepairDuplicate(t *testing.T) {
	frames := []*ingest.Frame{
		frame(t, "v1", 0, 0, true),
		frame(t, "v1", 1, 3000, true),
		frame(t, "v1", 1, 3000, true), // duplicate seq
	}
	rep := Repair(frames, videoClock(), "in.jsonl")
	if rep.Duplicates != 1 {
		t.Fatalf("duplicates=%d", rep.Duplicates)
	}
	if rep.Frames[2].MasterTS != rep.Frames[1].MasterTS || !rep.Frames[2].Inferred {
		t.Fatal("duplicate not aligned to first occurrence")
	}
}

func TestRepairChecksumFrameInferred(t *testing.T) {
	frames := []*ingest.Frame{
		frame(t, "v1", 0, 0, true),
		frame(t, "v1", 1, 999999, false), // bad checksum; ts must not be trusted
	}
	rep := Repair(frames, videoClock(), "in.jsonl")
	if !rep.Frames[1].Inferred || rep.Frames[1].MasterTS != 3000 {
		t.Fatalf("checksum-bad frame: %+v", rep.Frames[1])
	}
}
