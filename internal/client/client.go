// Package client drives one RTSP session end to end: it picks a transport,
// walks the shared FSM from OPTIONS to PLAY, feeds RTP into the
// reassemblers and collects the per-mode metrics. Only loopback targets
// are allowed.
package client

import (
	"fmt"
	"net"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	pionrtp "github.com/pion/rtp"

	"rtspreplay/internal/rtp"
	"rtspreplay/internal/rtsp"
	"rtspreplay/internal/sdp"
	"rtspreplay/internal/session"
)

// Options parameterizes one Run.
type Options struct {
	URL           string
	Transport     string // "udp" or "tcp"
	User          string
	Pass          string
	StateTimeout  time.Duration // per-state FSM guard
	IdleTimeout   time.Duration // stop when no RTP arrives for this long
	ReorderWindow int
	GapThreshold  int
}

// Result aggregates the metrics of one finished session.
type Result struct {
	Transitions int
	Gaps        int
	Frames      int
	Duplicates  int
	OutOfOrder  int
	Discarded   int
	FrameErrors int
}

// Run executes a full OPTIONS..TEARDOWN session and returns its metrics.
// Any failure funnels into TEARDOWN with all ports closed before the error
// is returned.
func Run(opts Options) (*Result, error) {
	addr, err := checkURL(opts.URL)
	if err != nil {
		return nil, err
	}
	var tr rtsp.Transport
	switch opts.Transport {
	case "udp":
		tr = rtsp.NewUDP(addr)
	case "tcp":
		tr = rtsp.NewInterleaved(addr)
	default:
		return nil, fmt.Errorf("client: unknown transport %q", opts.Transport)
	}

	fsm := session.New(opts.StateTimeout)
	res := &Result{}
	if err := run(fsm, tr, opts, res); err != nil {
		fsm.Teardown()
		_ = tr.Close()
		res.Transitions = fsm.Transitions()
		return res, err
	}
	res.Transitions = fsm.Transitions()
	return res, nil
}

func run(fsm *session.FSM, tr rtsp.Transport, opts Options, res *Result) error {
	if err := tr.Connect(); err != nil {
		return err
	}
	defer tr.Close()

	// INIT -> OPTIONS
	if err := fsm.Transition(session.StateOptions); err != nil {
		return err
	}
	if _, err := tr.Do(&base.Request{Method: base.Options, URL: mustURL(opts.URL)}); err != nil {
		return err
	}

	// OPTIONS -> DESCRIBE, with one digest-auth retry on 401.
	if err := fsm.Transition(session.StateDescribe); err != nil {
		return err
	}
	descReq := &base.Request{
		Method: base.Describe,
		URL:    mustURL(opts.URL),
		Header: base.Header{"Accept": base.HeaderValue{"application/sdp"}},
	}
	resp, err := tr.Do(descReq)
	if err != nil {
		return err
	}
	if resp.StatusCode == base.StatusUnauthorized {
		auth, err := rtsp.DigestAuthorization("DESCRIBE", opts.URL,
			opts.User, opts.Pass, headerGet(resp.Header, "www-authenticate"))
		if err != nil {
			return err
		}
		if err := fsm.Transition(session.StateDescribe); err != nil { // self-edge retry
			return err
		}
		descReq.Header["Authorization"] = base.HeaderValue{auth}
		if resp, err = tr.Do(descReq); err != nil {
			return err
		}
	}
	if resp.StatusCode != base.StatusOK {
		return fmt.Errorf("client: DESCRIBE status %d", resp.StatusCode)
	}
	desc, err := sdp.Parse(resp.Body, opts.URL)
	if err != nil {
		return err
	}

	// DESCRIBE -> SETUP: negotiate every track. PLAY is unreachable until
	// this loop succeeds, because the FSM has no DESCRIBE->PLAYING edge.
	if err := fsm.Transition(session.StateSetup); err != nil {
		return err
	}
	sessionID := ""
	for _, track := range desc.Tracks {
		if sessionID, err = tr.SetupTrack(track, sessionID); err != nil {
			return err
		}
	}

	// Wire the media plane before PLAY so no packet is missed.
	reams := make([]*rtp.Reassembler, len(desc.Tracks))
	framers := make([]*rtp.Framer, len(desc.Tracks))
	for i := range desc.Tracks {
		idx := i
		f, err := rtp.NewFramer()
		if err != nil {
			return err
		}
		framers[idx] = f
		reams[idx] = rtp.NewReassembler(opts.ReorderWindow, opts.GapThreshold,
			func(pkts []*pionrtp.Packet) { framers[idx].Assemble(pkts) })
	}
	var lastPkt atomic.Int64
	lastPkt.Store(time.Now().UnixNano())
	errCh := make(chan error, 1)
	err = tr.StartRTP(func(trackIdx int, pkt *pionrtp.Packet) {
		lastPkt.Store(time.Now().UnixNano())
		if trackIdx >= 0 && trackIdx < len(reams) {
			reams[trackIdx].Add(pkt)
		}
	}, func(err error) {
		select {
		case errCh <- err:
		default:
		}
	})
	if err != nil {
		return err
	}

	// SETUP -> PLAYING
	playRes, err := tr.Do(&base.Request{
		Method: base.Play,
		URL:    mustURL(opts.URL),
		Header: base.Header{"Session": base.HeaderValue{sessionID}},
	})
	if err != nil {
		return err
	}
	if playRes.StatusCode != base.StatusOK {
		return fmt.Errorf("client: PLAY status %d", playRes.StatusCode)
	}
	if err := fsm.Transition(session.StatePlaying); err != nil {
		return err
	}

	// Wait until the server disconnects, the stream goes idle or the FSM
	// guard fires. Incomplete frames are dropped either way; sealed frames
	// and counters are kept.
	var runErr error
	ticker := time.NewTicker(20 * time.Millisecond)
loop:
	for {
		select {
		case e := <-errCh:
			runErr = fmt.Errorf("client: media channel: %w", e)
			break loop
		case <-ticker.C:
			if fsm.Expired() {
				runErr = fmt.Errorf("client: state %s exceeded its guard", fsm.State())
				break loop
			}
			if time.Since(time.Unix(0, lastPkt.Load())) > opts.IdleTimeout {
				break loop
			}
		}
	}
	ticker.Stop()

	// Any exit lands in TEARDOWN; ports are released by Close.
	fsm.Teardown()
	tr.Teardown(sessionID)
	_ = tr.Close() // also joins the reader goroutines
	for _, r := range reams {
		r.Flush() // drop the unfinished tail frame
	}
	for _, r := range reams {
		st := r.Stats()
		res.Gaps += st.Gaps
		res.Duplicates += st.Duplicates
		res.OutOfOrder += st.OutOfOrder
		res.Discarded += st.Discarded
	}
	for _, f := range framers {
		res.Frames += f.Frames
		res.FrameErrors += f.FrameErrors
	}
	return runErr
}

// checkURL enforces the loopback-only constraint and returns host:port.
func checkURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("client: bad URL: %w", err)
	}
	if u.Scheme != "rtsp" {
		return "", fmt.Errorf("client: unsupported scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return "", fmt.Errorf("client: only 127.0.0.1 targets are allowed, got %q", host)
	}
	port := u.Port()
	if port == "" {
		port = "554"
	}
	return net.JoinHostPort(host, port), nil
}

func mustURL(raw string) *base.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	bu := base.URL(*u)
	return &bu
}

// headerGet finds a header value case-insensitively.
func headerGet(h base.Header, key string) string {
	for k, v := range h {
		if equalFoldASCII(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
