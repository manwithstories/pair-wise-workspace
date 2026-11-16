package peer

import (
	"errors"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// waitConnected retries the connect path a few times because ICE is timing
// sensitive under a loaded CI machine; a single failure is not evidence of a
// broken test.
func connectWithRetry(t *testing.T, cfg Config) *Pair {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		p, err := Connect(cfg)
		if err == nil {
			return p
		}
		lastErr = err
	}
	t.Fatalf("connect failed after 3 attempts: %v", lastErr)
	return nil
}

func TestConnectUsesOnlyLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real peer connections")
	}
	p := connectWithRetry(t, Config{ConnectTimeout: 10 * time.Second})
	defer func() { _ = p.Close() }()

	local, remote := p.SelectedPair()
	if local == "" || remote == "" {
		t.Fatalf("selected pair is empty: %q <-> %q", local, remote)
	}
	if !p.AllLoopback() {
		t.Fatalf("non-loopback candidate gathered: %+v", p.Candidates())
	}
	cands := p.Candidates()
	if len(cands) < 2 {
		t.Fatalf("want candidates from both sides, got %+v", cands)
	}
	sides := map[string]bool{}
	for _, c := range cands {
		sides[c.Side] = true
	}
	if !sides["sender"] || !sides["receiver"] {
		t.Fatalf("candidates missing a side: %+v", cands)
	}
}

func TestPinnedSSRCReachesReceiver(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real peer connections")
	}
	const want = uint32(0xC0FFEE01)
	p := connectWithRetry(t, Config{ConnectTimeout: 10 * time.Second, SSRC: want})
	defer func() { _ = p.Close() }()

	seen := make(chan uint32, 1)
	done := make(chan error, 1)
	go func() {
		done <- p.ReadLoop(5*time.Second, func(pkt *rtp.Packet) error {
			select {
			case seen <- pkt.SSRC:
			default:
			}
			return nil
		})
	}()

	// Give the reader time to attach, then send one packet.
	time.Sleep(200 * time.Millisecond)
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, SSRC: 0xDEADBEEF},
		Payload: []byte("ssrc probe"),
	}
	if err := p.WriteRTP(pkt); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-seen:
		if got != want {
			t.Fatalf("receiver saw SSRC %#x, want the pinned %#x", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no packet arrived (reader error: %v)", <-done)
	}
	_ = p.Close()
}

func TestCloseIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real peer connections")
	}
	p := connectWithRetry(t, Config{ConnectTimeout: 10 * time.Second})
	if err := p.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second close must be a no-op: %v", err)
	}
	if err := p.WritePacket([]byte{0x80}); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: want ErrClosed, got %v", err)
	}
}
