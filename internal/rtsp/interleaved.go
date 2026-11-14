package rtsp

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/pion/rtp"

	"rtspreplay/internal/sdp"
)

// errBadFrame marks a skipped, malformed $ frame. It is counted once per
// bad frame and never terminates the read loop.
var errBadFrame = errors.New("rtsp: malformed interleaved frame")

// Interleaved is the RTP-over-RTSP (TCP) transport for devices behind NAT.
// Media travels inside the control connection as $ frames.
type Interleaved struct {
	addr     string
	cc       *controlConn
	channels map[int]int // RTP channel -> track index
	closed   atomic.Bool
	wg       sync.WaitGroup

	// FrameErrors counts malformed $ frames, once each.
	FrameErrors int64
}

// NewInterleaved returns a TCP-interleaved transport for host:port.
func NewInterleaved(addr string) *Interleaved {
	return &Interleaved{addr: addr, channels: make(map[int]int)}
}

// Connect dials the control connection.
func (t *Interleaved) Connect() error {
	cc, err := dialControl(t.addr)
	if err != nil {
		return err
	}
	t.cc = cc
	return nil
}

// Do sends one request on the shared control connection.
func (t *Interleaved) Do(req *base.Request) (*base.Response, error) { return t.cc.do(req) }

// SetupTrack negotiates one track over channel pair 2i/2i+1.
func (t *Interleaved) SetupTrack(track *sdp.Track, sessionID string) (string, error) {
	rtpCh := 2 * len(t.channels)
	req := &base.Request{
		Method: base.Setup,
		URL:    mustParseURL(track.URL),
		Header: base.Header{
			"Transport": base.HeaderValue{fmt.Sprintf(
				"RTP/AVP/TCP;unicast;interleaved=%d-%d", rtpCh, rtpCh+1)},
		},
	}
	if sessionID != "" {
		req.Header["Session"] = base.HeaderValue{sessionID}
	}
	res, err := t.Do(req)
	if err != nil {
		return "", err
	}
	if res.StatusCode != base.StatusOK {
		return "", fmt.Errorf("rtsp: SETUP %s: status %d", track.URL, res.StatusCode)
	}
	t.channels[rtpCh] = track.Index
	return parseSessionID(res), nil
}

// StartRTP runs the single $ frame read loop.
func (t *Interleaved) StartRTP(onPkt PacketHandler, onErr ErrorHandler) error {
	fr := &frameReader{nc: t.cc.nc}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		for {
			ch, payload, err := fr.next()
			if err != nil {
				if errors.Is(err, errBadFrame) {
					atomic.AddInt64(&t.FrameErrors, 1)
					continue
				}
				if !t.closed.Load() && onErr != nil {
					onErr(err) // e.g. server disconnected mid-stream
				}
				return
			}
			idx, ok := t.channels[ch]
			if !ok {
				if ch%2 == 0 { // invalid RTP channel: count once, keep going
					atomic.AddInt64(&t.FrameErrors, 1)
				}
				continue
			}
			pkt := parseRTPPacket(payload)
			if pkt == nil {
				atomic.AddInt64(&t.FrameErrors, 1)
				continue
			}
			onPkt(idx, pkt)
		}
	}()
	return nil
}

// Teardown sends a best-effort TEARDOWN without waiting for the response:
// in-flight $ frames make a synchronous read unreliable.
func (t *Interleaved) Teardown(sessionID string) {
	if t.cc == nil {
		return
	}
	t.cc.writeFireAndForget(&base.Request{
		Method: base.Teardown,
		URL:    mustParseURL("rtsp://" + t.addr + "/"),
		Header: base.Header{"Session": base.HeaderValue{sessionID}},
	})
}

// Close shuts the connection and waits for the read loop.
func (t *Interleaved) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	if t.cc != nil {
		t.cc.close()
	}
	t.wg.Wait()
	return nil
}

// frameReader reassembles $ frames from a byte stream, explicitly stitching
// headers and payloads split across multiple Read calls.
type frameReader struct {
	nc  net.Conn
	buf []byte
}

func (r *frameReader) fill() error {
	tmp := make([]byte, 4096)
	n, err := r.nc.Read(tmp)
	if n > 0 {
		r.buf = append(r.buf, tmp[:n]...)
	}
	return err
}

// next returns the channel and payload of the next complete frame. A
// malformed frame is skipped and reported as errBadFrame exactly once.
func (r *frameReader) next() (int, []byte, error) {
	for {
		for len(r.buf) < 4 {
			if err := r.fill(); err != nil {
				return 0, nil, err
			}
		}
		if r.buf[0] != 0x24 { // not frame magic: drop one byte to resync
			r.buf = r.buf[1:]
			return 0, nil, errBadFrame
		}
		ch := int(r.buf[1])
		l := int(r.buf[2])<<8 | int(r.buf[3])
		for len(r.buf) < 4+l {
			if err := r.fill(); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, l)
		copy(payload, r.buf[4:4+l])
		r.buf = r.buf[4+l:]
		return ch, payload, nil
	}
}

// parseRTPPacket unmarshals one RTP packet, returning nil on bad input.
func parseRTPPacket(raw []byte) *rtp.Packet {
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		return nil
	}
	return pkt
}
