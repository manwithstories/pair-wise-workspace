package transport

import (
	"errors"
	"testing"
	"time"

	"mediad/internal/model"
)

func packetsForTest(t *testing.T, stream string, chunks ...string) []Packet {
	t.Helper()
	var off int64
	out := make([]Packet, len(chunks))
	for i, chunk := range chunks {
		out[i] = NewPacket(stream, uint64(i), off, []byte(chunk))
		off += int64(len(chunk))
	}
	return out
}

func TestDuplicateIdenticalAndConflict(t *testing.T) {
	pk := packetsForTest(t, "s", "zero", "one")
	a := NewAssembler(DefaultConfig("s"))
	if err := a.Handle(pk[0]); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(pk[0]); err != nil {
		t.Fatalf("identical duplicate must be idempotent: %v", err)
	}
	conflict := NewPacket("s", 0, pk[0].Offset, []byte("different"))
	err := a.Handle(conflict)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeDuplicateConflict {
		t.Fatalf("expected duplicate_conflict, got %v", err)
	}
	if got := a.Stats().Duplicate; got != 1 {
		t.Fatalf("duplicate=%d, want 1", got)
	}
}

func TestReorderBurstLossAndRetransmit(t *testing.T) {
	pk := packetsForTest(t, "stream", "000", "111", "222", "333", "444", "555")
	cfg := DefaultConfig("stream")
	cfg.RTT = 100 * time.Millisecond
	cfg.BandwidthBps = 8_000_000
	a := NewAssembler(cfg)

	order := []int{0, 3, 5, 4}
	for _, i := range order {
		if err := a.Handle(pk[i]); err != nil {
			t.Fatal(err)
		}
	}
	st := a.Stats()
	if st.Reordered != 3 {
		t.Fatalf("reordered=%d want 3", st.Reordered)
	}
	if st.BurstLoss != 2 {
		t.Fatalf("burst_loss=%d want 2", st.BurstLoss)
	}
	req, err := a.RetransmitRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(req) != 2 || req[0] != 1 || req[1] != 2 {
		t.Fatalf("requests=%v want [1 2]", req)
	}
	if err := a.Handle(pk[1]); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(pk[2]); err != nil {
		t.Fatal(err)
	}
	got, err := a.Assemble(len(pk))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "000111222333444555" {
		t.Fatalf("assembled=%q", got)
	}
	if a.Stats().Retransmit != 2 {
		t.Fatalf("retransmit=%d want 2", a.Stats().Retransmit)
	}
}

func TestRetransmitExhaustionRejectsIncomplete(t *testing.T) {
	pk := packetsForTest(t, "s", "first", "second")
	cfg := DefaultConfig("s")
	cfg.MaxRetries = 2
	a := NewAssembler(cfg)
	if err := a.Handle(pk[1]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cfg.MaxRetries; i++ {
		if _, err := a.RetransmitRequests(); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	_, err := a.RetransmitRequests()
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeRetryExhausted {
		t.Fatalf("expected retry_exhausted, got %v", err)
	}
	if model.ExitCode(err) != 4 {
		t.Fatalf("exit code=%d want 4", model.ExitCode(err))
	}
	if _, err := a.Assemble(2); err == nil {
		t.Fatal("incomplete stream must not commit")
	}
}

func TestBandwidthChangesRetransmitWindow(t *testing.T) {
	cfg := DefaultConfig("s")
	cfg.MaxPacketSize = 1000
	cfg.RTT = 100 * time.Millisecond
	cfg.MaxWindowPackets = 1000
	a := NewAssembler(cfg)
	low := a.SetBandwidth(80_000) // 1000 bytes/RTT => one packet
	high := a.SetBandwidth(8_000_000)
	if low != 1 {
		t.Fatalf("low window=%d want 1", low)
	}
	if high <= low {
		t.Fatalf("window did not grow: low=%d high=%d", low, high)
	}
}

func TestPacketChecksumDetectsCorruption(t *testing.T) {
	p := NewPacket("s", 0, 0, []byte("payload"))
	p.Data[0] ^= 0xff
	if err := p.ValidateChecksum(); err == nil {
		t.Fatal("expected checksum failure")
	}
}
