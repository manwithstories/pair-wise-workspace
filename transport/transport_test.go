package transport

import (
	"context"
	"errors"
	"testing"

	"github.com/pion/rtp"

	"fieldpost/internal/core"
)

func pkt(seq uint16, payload byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: []byte{payload}}
}

func TestSeqDeltaWrap(t *testing.T) {
	if d := SeqDelta(65535, 0); d != 1 {
		t.Fatalf("wrap delta=%d", d)
	}
	if d := SeqDelta(0, 65535); d != -1 {
		t.Fatalf("reverse wrap delta=%d", d)
	}
}

func TestRecoverDeduplicatesReordersAndWraps(t *testing.T) {
	base := uint16(65533)
	in := []*rtp.Packet{
		pkt(1, 4), pkt(65533, 0), pkt(0, 3),
		pkt(65534, 1), pkt(65535, 2), pkt(1, 4), pkt(65534, 1),
	}
	got, rep, err := RecoverSegment(context.Background(), "cam-a-7", base, 5, in, nil, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || string([]byte{got[0][0], got[1][0], got[2][0], got[3][0], got[4][0]}) != "\x00\x01\x02\x03\x04" {
		t.Fatalf("ordered payloads=%v", got)
	}
	if rep.Duplicates != 2 || rep.Reordered != 4 {
		t.Fatalf("report=%+v", rep)
	}
}

func TestMissingPacketRetriesLocalSourceTwiceThenSucceeds(t *testing.T) {
	calls := map[uint16]int{}
	src := func(_ context.Context, seq uint16) (*rtp.Packet, error) {
		calls[seq]++
		if calls[seq] < 2 {
			return nil, errors.New("local shard busy")
		}
		return pkt(seq, 9), nil
	}
	in := []*rtp.Packet{pkt(10, 1), pkt(12, 3)}
	got, rep, err := RecoverSegment(context.Background(), "seg-retry", 10, 3, in, src, 8)
	if err != nil {
		t.Fatal(err)
	}
	if calls[11] != MaxRetries || rep.Retries != 2 || rep.Recovered != 1 {
		t.Fatalf("calls=%v report=%+v", calls, rep)
	}
	if got[1][0] != 9 {
		t.Fatalf("retry payload=%v", got[1])
	}
}

func TestPersistentGapIsolatesWithLocatableError(t *testing.T) {
	calls := 0
	src := func(context.Context, uint16) (*rtp.Packet, error) {
		calls++
		return nil, errors.New("not in local source")
	}
	in := []*rtp.Packet{pkt(20, 1), pkt(22, 3)}
	_, rep, err := RecoverSegment(context.Background(), "lost-burst", 20, 3, in, src, 8)
	if err == nil {
		t.Fatal("expected isolation error")
	}
	if calls != 2 || rep.Retries != 2 || len(rep.Missing) != 1 || rep.Missing[0] != 21 {
		t.Fatalf("calls=%d report=%+v", calls, rep)
	}
	if core.ExitCode(err) != core.ExitUnrecoverable {
		t.Fatalf("exit=%d", core.ExitCode(err))
	}
	fe, ok := core.AsError(err)
	if !ok || fe.Segment != "lost-burst" || fe.Offset != 21 || fe.Retryable {
		t.Fatalf("error not locatable/non-retryable: %#v", fe)
	}
}

func TestBoundedWindowRejectsFarPacket(t *testing.T) {
	b := NewReorderBuffer(100, 8)
	if r := b.Add(pkt(102, 1)); r != Added {
		t.Fatalf("delta2 result=%v", r)
	}
	if r := b.Add(pkt(120, 9)); r != OutsideWindow {
		t.Fatalf("far result=%v", r)
	}
	if b.Buffered() > int(DefaultWindow) {
		t.Fatalf("buffer exceeded window: %d", b.Buffered())
	}
}

func TestParseAndAddUsesPionRTP(t *testing.T) {
	p := pkt(7, 0xaa)
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b := NewReorderBuffer(7, 4)
	res, err := b.ParseAndAdd(raw)
	if err != nil || res != Added {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if _, err := b.ParseAndAdd([]byte{0x80, 0x01}); err == nil {
		t.Fatal("truncated RTP accepted")
	}
}
