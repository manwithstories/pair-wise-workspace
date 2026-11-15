package rtsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	pionrtp "github.com/pion/rtp"

	auditrtp "rtspaudit/internal/rtp"
)

// PullOptions controls one audit stream.
type PullOptions struct {
	Transport      string
	SilenceTimeout time.Duration
	MaxReconnects  int
}

// PullResult contains bounded counters gathered during one transport run.
type PullResult struct {
	Transport          string
	StateTransitions   int
	IllegalTransitions int
	DuplicatePackets   uint64
	LateDropped        uint64
	RTPSeqGaps         []auditrtp.Gap
	Depacket           auditrtp.DepacketStats
	Reconnects         int
}

type pullSession struct {
	transport Transport
	url       string
	sessionID string
	cseq      int
}

func (s *pullSession) do(ctx context.Context, method, rawURL string, headers map[string]string) (Response, error) {
	s.cseq++
	cseq := strconv.Itoa(s.cseq)
	if err := s.transport.SendControl(ctx, buildRequest(method, rawURL, cseq, headers)); err != nil {
		return Response{}, err
	}
	data, err := s.transport.ReceiveControl(ctx)
	if err != nil {
		return Response{}, err
	}
	resp, err := ParseResponse(data)
	if err != nil {
		return Response{}, err
	}
	if resp.Headers["cseq"] != cseq {
		return Response{}, fmt.Errorf("%w: CSeq mismatch, want %s got %q", ErrProtocol, cseq, resp.Headers["cseq"])
	}
	return resp, nil
}

func expectOK(machine *Machine, event Event, resp Response, method string) error {
	if resp.StatusCode == 454 {
		machine.Apply(EventSessionTimeout)
		return fmt.Errorf("%w: %s returned 454", ErrSessionTimeout, method)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%w: %s returned %d %s", ErrProtocol, method, resp.StatusCode, resp.Status)
	}
	machine.Apply(event)
	return nil
}

// Pull performs the canonical OPTIONS/DESCRIBE/SETUP/PLAY/TEARDOWN exchange.
func Pull(ctx context.Context, addr string, opts PullOptions) (result PullResult, err error) {
	kind := strings.ToLower(opts.Transport)
	var transport Transport
	switch kind {
	case "tcp":
		transport, err = DialTCP(ctx, addr)
	case "udp":
		transport, err = DialUDP(ctx, addr, opts.SilenceTimeout)
	default:
		return result, fmt.Errorf("unknown transport %q", opts.Transport)
	}
	if err != nil {
		return result, err
	}
	defer transport.Close()

	machine := NewMachine()
	var tracker *auditrtp.SeqTracker
	depacket := auditrtp.NewDepacketizer()
	reconnects := 0
	defer func() {
		result.Transport = kind
		result.StateTransitions = machine.TransitionCount()
		result.IllegalTransitions = machine.IllegalTransitions()
		result.Reconnects = reconnects
		if tracker != nil {
			result.DuplicatePackets = tracker.Duplicates()
			result.LateDropped = tracker.LateDropped()
			result.RTPSeqGaps = tracker.Gaps()
		}
		result.Depacket = depacket.Stats()
	}()

	baseURL := "rtsp://" + addr + "/audit"
	trackURL := baseURL + "/trackID=0"
	sess := &pullSession{transport: transport, url: trackURL}
	resp, err := sess.do(ctx, "OPTIONS", baseURL, nil)
	if err != nil {
		return result, wrapProtocol(err)
	}
	if err := expectOK(machine, EventOPTIONS, resp, "OPTIONS"); err != nil {
		return result, err
	}

	resp, err = sess.do(ctx, "DESCRIBE", baseURL, map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return result, wrapProtocol(err)
	}
	if resp.StatusCode == 200 {
		if err := validateSDP(resp.Body); err != nil {
			return result, err
		}
	}
	if err := expectOK(machine, EventDESCRIBE, resp, "DESCRIBE"); err != nil {
		return result, err
	}

	setupHeaders := make(map[string]string)
	if kind == "tcp" {
		setupHeaders["Transport"] = "RTP/AVP/TCP;unicast;interleaved=0-1"
	} else {
		rtpPort, rtcpPort := transport.(*UDPTransport).Ports()
		setupHeaders["Transport"] = fmt.Sprintf("RTP/AVP;unicast;client_port=%d-%d", rtpPort, rtcpPort)
	}
	resp, err = sess.do(ctx, "SETUP", trackURL, setupHeaders)
	if err != nil {
		return result, wrapProtocol(err)
	}
	if resp.StatusCode != 200 {
		if err := expectOK(machine, EventSETUP, resp, "SETUP"); err != nil {
			return result, err
		}
	}
	if err := validateSetup(kind, transport, resp); err != nil {
		return result, err
	}
	sess.sessionID = sessionID(resp.Headers["session"])
	if sess.sessionID == "" {
		return result, fmt.Errorf("%w: SETUP response has no Session", ErrProtocol)
	}
	if err := expectOK(machine, EventSETUP, resp, "SETUP"); err != nil {
		return result, err
	}

	play := func() (uint16, bool, error) {
		resp, err := sess.do(ctx, "PLAY", trackURL, map[string]string{"Session": sess.sessionID})
		if err != nil {
			return 0, false, wrapProtocol(err)
		}
		if err := expectOK(machine, EventPLAY, resp, "PLAY"); err != nil {
			return 0, false, err
		}
		seq, ok := rtpInfoSeq(resp.Headers["rtp-info"])
		return seq, ok, nil
	}
	seq, hasSeq, err := play()
	if err != nil {
		return result, err
	}
	if hasSeq {
		tracker = auditrtp.NewSeqTrackerAt(seq, 32)
	}

	for {
		data, recvErr := transport.ReceiveMedia(ctx)
		if recvErr == nil {
			var packet pionrtp.Packet
			if err := packet.Unmarshal(data); err != nil {
				return result, fmt.Errorf("%w: invalid RTP packet: %v", ErrProtocol, err)
			}
			if tracker == nil {
				tracker = auditrtp.NewSeqTracker(32)
			}
			ordered, _, _ := tracker.Push(&packet)
			for _, item := range ordered {
				if err := depacket.PushOrdered(item); err != nil {
					return result, fmt.Errorf("%w: %v", ErrProtocol, err)
				}
			}
			continue
		}
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if errors.Is(recvErr, ErrUDPSilence) {
			machine.Apply(EventUDPSilence)
			if reconnects >= opts.MaxReconnects {
				machine.Apply(EventTransportDisconnect)
				return result, ErrSilenceExhausted
			}
			reconnects++
			_, _, err := play()
			if err != nil {
				return result, err
			}
			continue
		}
		machine.Apply(EventTransportDisconnect)
		return result, fmt.Errorf("receive media: %w", recvErr)
	}

	if tracker != nil {
		for _, item := range tracker.Flush() {
			if err := depacket.PushOrdered(item); err != nil {
				return result, fmt.Errorf("%w: %v", ErrProtocol, err)
			}
		}
	}
	depacket.Finish()

	resp, err = sess.do(ctx, "TEARDOWN", trackURL, map[string]string{"Session": sess.sessionID})
	if err != nil {
		return result, wrapProtocol(err)
	}
	if err := expectOK(machine, EventTEARDOWN, resp, "TEARDOWN"); err != nil {
		return result, err
	}
	return result, nil
}

func validateSetup(kind string, transport Transport, resp Response) error {
	if resp.StatusCode != 200 {
		return nil
	}
	value := resp.Headers["transport"]
	if value == "" {
		return fmt.Errorf("%w: SETUP response has no Transport", ErrProtocol)
	}
	params, err := ParseTransportParams(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if kind == "tcp" && !strings.Contains(strings.ToUpper(params.Protocol), "TCP") {
		return fmt.Errorf("%w: expected TCP transport, got %s", ErrProtocol, params.Protocol)
	}
	if kind == "udp" {
		if err := transport.(*UDPTransport).ApplyServerTransport(value); err != nil {
			return fmt.Errorf("%w: %v", ErrProtocol, err)
		}
	}
	return nil
}

func wrapProtocol(err error) error {
	if errors.Is(err, ErrProtocol) || errors.Is(err, ErrSessionTimeout) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrProtocol, err)
}
