package rtsp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	rtppkg "github.com/example/rtpcap/internal/rtp"
	"github.com/example/rtpcap/internal/scenario"
	"github.com/example/rtpcap/internal/store"
)

// RejectNonLoopback enforces the rule that capture only ever talks to a
// loopback RTSP address. It returns ExitNonLoopback for any other host.
func RejectNonLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback()) {
		return &NonLoopbackError{Addr: addr}
	}
	return nil
}

// NonLoopbackError maps to process exit code 5.
type NonLoopbackError struct{ Addr string }

func (e *NonLoopbackError) Error() string {
	return "refusing to connect to non-loopback address: " + e.Addr
}

// CaptureResult summarizes one capture run for report building.
type CaptureResult struct {
	Stats      rtppkg.Stats
	Gaps       []rtppkg.GapInterval
	Reconnects int
	SM         *StateMachine
	Err        error
}

// Run performs the full capture: handshake, packet loop, frame persistence,
// automatic reconnect on stream cuts, and orderly PAUSE/TEARDOWN. It is
// identical for both transports; only the SETUP negotiation differs.
func Run(ctx context.Context, sc *scenario.Scenario, mode, serverAddr, mediaURL string,
	fs *store.FrameStore, logger *slog.Logger) CaptureResult {

	sm := NewStateMachine()
	dep := rtppkg.NewDepacketizer(64)
	dep.SetFrameOffset(fs.NextFrameIndex())
	res := CaptureResult{SM: sm}

	readTimeout := time.Duration(sc.ReadTimeoutMS) * time.Millisecond
	expectedLast := sc.TotalPackets - 1

	client := &Client{
		ServerAddr: serverAddr,
		MediaURL:   mediaURL,
		Username:   sc.Username,
		Password:   sc.Password,
		SM:         sm,
	}
	if err := client.Dial(); err != nil {
		res.Err = err
		return res
	}
	if err := client.Handshake(mode, readTimeout); err != nil {
		res.Err = err
		client.Close()
		return res
	}

	resumeSeq := 0
	if last := fs.LastContiguousSeq(); last >= 0 {
		// The store holds raw RTP sequence numbers; the server resume parameter
		// is a logical packet position, so convert via the scenario start seq.
		resumeSeq = rtpSeqToLogical(sc, uint16(last+1))
	}
	if err := client.Play(resumeSeq); err != nil {
		res.Err = classify(err)
		client.Close()
		return res
	}

	// The store already holds every packet of the stream: nothing remains to
	// capture, so finish successfully without entering the read loop.
	if resumeSeq >= sc.TotalPackets {
		_ = client.Pause()
		_ = client.Teardown()
		res.Stats, res.Gaps = dep.Finalize()
		return res
	}

	tr := client.RTPTransport()
	idleLoops := 0
	maxIdleLoops := 3
	// Only a connection started from the beginning can hit the scripted cut.
	// A resumed connection runs past the cut point uninterrupted.
	cutPending := shouldReconnect(sc) && resumeSeq == 0

	for {
		select {
		case <-ctx.Done():
			res.Err = &TransportError{Detail: ctx.Err().Error()}
			goto finalize
		default:
		}
		raw, err := tr.ReadPacket()
		if err != nil {
			var rto ReadTimeout
			if errors.As(err, &rto) {
				idleLoops++
				// Over UDP a cut is silent: the data path just goes quiet.
				// When the scenario scripts a cut and the stream is incomplete,
				// treat a sustained idle period as the cut and reconnect.
				if cutPending && !dep.Reached(uint32(sc.TotalPackets-1)) {
					var rcErr error
					client, tr, rcErr = reconnect(client, sm, sc, mode,
						serverAddr, mediaURL, readTimeout, dep, logger, &res)
					if rcErr != nil {
						res.Err = rcErr
						goto finalize
					}
					idleLoops = 0
					continue
				}
				if idleLoops >= maxIdleLoops {
					res.Err = rto
					goto finalize
				}
				continue
			}
			if !cutPending {
				res.Err = classify(err)
				goto finalize
			}
			var rcErr error
			client, tr, rcErr = reconnect(client, sm, sc, mode,
				serverAddr, mediaURL, readTimeout, dep, logger, &res)
			if rcErr != nil {
				res.Err = rcErr
				goto finalize
			}
			idleLoops = 0
			cutPending = false
			continue
		}
		idleLoops = 0

		frames, pushErr := dep.Push(raw)
		if pushErr != nil {
			logger.Warn("malformed rtp packet ignored", "err", pushErr)
			continue
		}
		for i := range frames {
			if writeErr := fs.WriteFrame(frames[i]); writeErr != nil {
				res.Err = &TransportError{Detail: writeErr.Error()}
				goto finalize
			}
		}
		if streamComplete(dep, sc, expectedLast) {
			break
		}
	}

	if fr := dep.Flush(); fr != nil {
		if err := fs.WriteFrame(*fr); err != nil {
			res.Err = &TransportError{Detail: err.Error()}
			goto finalize
		}
	}
	_ = client.Pause()

finalize:
	_ = client.Teardown()
	res.Stats, res.Gaps = dep.Finalize()
	return res
}

func streamComplete(dep *rtppkg.Depacketizer, sc *scenario.Scenario, expectedLast int) bool {
	st := dep.Stats()
	if !st.HaveBounds {
		return false
	}
	// Signed modular distance: >= 0 means the observed high-water seq has
	// reached or passed the wanted final sequence, including wrap-around.
	want := sc.SeqAt(expectedLast)
	return int16(st.LastSeq-want) >= 0
}

func shouldReconnect(sc *scenario.Scenario) bool {
	cut := sc.EventOf(scenario.EventCut)
	// A cut explicitly marked non-resumable (ResumeAt < 0) surfaces as a hard
	// transport/read failure instead of triggering an automatic reconnect.
	return cut != nil && cut.ResumeAt >= 0
}

func classify(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout"):
		return ReadTimeout{}
	case errors.Is(err, io.EOF) || strings.Contains(msg, "EOF"):
		return &TransportError{Detail: "server closed the stream (teardown or cut)"}
	default:
		return err
	}
}

// rtpSeqToLogical converts a raw 16-bit RTP sequence number to the scenario's
// logical packet position (0-based), accounting for the configured start
// sequence and wrap-around.
func rtpSeqToLogical(sc *scenario.Scenario, seq uint16) int {
	delta := int32(int16(seq - uint16(sc.StartSeq)))
	if delta < 0 {
		return 0
	}
	return int(delta)
}

// reconnect tears down the broken transport, opens a fresh RTSP connection and
// resumes at the depacketizer frontier. It is shared by the TCP cut path
// (connection error) and the UDP cut path (sustained read idle).
func reconnect(client *Client, sm *StateMachine, sc *scenario.Scenario, mode,
	serverAddr, mediaURL string, readTimeout time.Duration,
	dep *rtppkg.Depacketizer, logger *slog.Logger, res *CaptureResult) (*Client, Transport, error) {

	logger.Info("stream cut, reconnecting", "reconnect", res.Reconnects+1)
	sm.ResetForReconnect()
	client.Close()

	next := &Client{
		ServerAddr: serverAddr, MediaURL: mediaURL,
		Username: sc.Username, Password: sc.Password, SM: sm,
	}
	if err := next.Dial(); err != nil {
		return nil, nil, err
	}
	if err := next.Handshake(mode, readTimeout); err != nil {
		next.Close()
		return nil, nil, err
	}
	resumeSeq := rtpSeqToLogical(sc, dep.NextSeq())
	if err := next.Play(resumeSeq); err != nil {
		next.Close()
		return nil, nil, classify(err)
	}
	res.Reconnects++
	return next, next.RTPTransport(), nil
}
