package ts_test

import (
	"bytes"
	"testing"

	"avpipe/internal/media"
	"avpipe/internal/ts"
)

const (
	pmtPID   = 0x1000
	videoPID = 0x0100
	audioPID = 0x0101
)

func TestPacketParsePCRAndPayload(t *testing.T) {
	pcr := uint64(90_000*42+123)*300 + 27
	payload := []byte("avpipe-idr")
	raw := ts.BuildMediaPacket(0x0100, 7, true, payload, &pcr, true, true)
	p, err := ts.ParsePacket(raw, 960, 5)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 0x0100 || p.CC != 7 || !p.PUSI {
		t.Fatalf("header mismatch: %+v", p)
	}
	if !p.HasPCR || p.PCR27 != pcr {
		t.Fatalf("PCR mismatch got %d want %d", p.PCR27, pcr)
	}
	if !p.RandomAccessIndicator || !p.DiscontinuityIndicator {
		t.Fatalf("adaptation flags not parsed: %+v", p)
	}
	if !bytes.HasPrefix(p.Payload, payload) {
		t.Fatalf("payload prefix mismatch: %q", p.Payload[:len(payload)])
	}
}

func TestParseStreamTruncatedTailIsNotPacket(t *testing.T) {
	pkt := ts.BuildMediaPacket(ts.NullPID, 0, false, make([]byte, 180), nil, false, false)
	input := append(append([]byte(nil), pkt...), pkt...)
	input = append(input, make([]byte, 37)...)
	packets, st, err := ts.ParseStream(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 2 || st.PacketCount != 2 {
		t.Fatalf("packets=%d stats=%d", len(packets), st.PacketCount)
	}
	if st.IncompleteTail != 1 || st.IncompleteBytes != 37 {
		t.Fatalf("tail stats=%+v", st)
	}
	if len(packets[1].Payload) == 0 || len(packets[1].Payload) > 184 {
		t.Fatalf("payload exceeds fixed packet bounds: %d", len(packets[1].Payload))
	}
}

func TestFirstSyncOffsetReported(t *testing.T) {
	pkt := ts.BuildMediaPacket(ts.NullPID, 0, false, make([]byte, 10), nil, false, false)
	input := append([]byte{0, 1, 2, 3}, pkt...)
	_, st, err := ts.ParseStream(input)
	if err != nil {
		t.Fatal(err)
	}
	if st.FirstSyncOffset != 4 || st.FirstDesyncOffset != 0 {
		t.Fatalf("sync stats=%+v", st)
	}
}

func TestCCTrackerReportsOneGapAndResyncs(t *testing.T) {
	tr := ts.NewCCTracker()
	seq := []uint8{0, 1, 4, 5}
	var gaps int
	for _, cc := range seq {
		if g := tr.Check(ts.Packet{PID: 0x100, CC: cc}); g != nil {
			gaps++
		}
	}
	if gaps != 1 || tr.GapCount() != 1 {
		t.Fatalf("gaps=%d records=%d", gaps, tr.GapCount())
	}
	if tr.Gaps[0].Expected != 2 || tr.Gaps[0].Got != 4 {
		t.Fatalf("unexpected gap: %+v", tr.Gaps[0])
	}
}

func feedSection(t *testing.T, state *ts.PSIState, pid uint16, section []byte, cc byte) {
	t.Helper()
	asm := ts.NewSectionAssembler()
	for _, raw := range ts.PacketizeSection(pid, section, cc) {
		p, err := ts.ParsePacket(raw, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, tab := range asm.HandlePacket(p, state) {
			state.Apply(tab)
		}
	}
}

func TestPSIValidVersionsAndBadCRCKeepsOldTable(t *testing.T) {
	state := ts.NewPSIState()
	feedSection(t, state, ts.PATPID, ts.BuildPAT(1, pmtPID), 0)
	if state.PAT == nil || state.PAT.PMTPID != pmtPID {
		t.Fatalf("PAT not committed: %+v", state.PAT)
	}
	feedSection(t, state, pmtPID, ts.BuildPMT(1, videoPID, videoPID, audioPID), 0)
	if state.PMTs[pmtPID] == nil {
		t.Fatal("PMT v1 not committed")
	}

	bad := ts.BuildPMT(2, videoPID, videoPID, audioPID)
	bad[len(bad)-2] ^= 0x01
	feedSection(t, state, pmtPID, bad, 1)
	if state.CRCRejectCount() != 1 {
		t.Fatalf("CRC rejects=%d", state.CRCRejectCount())
	}
	if got := state.PMTs[pmtPID].Version; got != 1 {
		t.Fatalf("bad CRC replaced table version=%d, want 1", got)
	}

	feedSection(t, state, pmtPID, ts.BuildPMT(3, videoPID, videoPID, audioPID), 2)
	if got := state.PMTs[pmtPID].Version; got != 3 {
		t.Fatalf("valid later version=%d, want 3", got)
	}
}

func TestPSIOutOfBoundsLengthRejected(t *testing.T) {
	state := ts.NewPSIState()
	asm := ts.NewSectionAssembler()
	p := ts.Packet{
		PID: pmtPID, PUSI: true, HasPayload: true, Offset: 17,
		Payload: []byte{0x00, 0x02, 0x07, 0xd0, 0x00},
	}
	asm.HandlePacket(p, state)
	if len(state.Rejections) != 1 || state.Rejections[0].Reason != media.CodePSILength {
		t.Fatalf("rejections=%+v", state.Rejections)
	}
}
