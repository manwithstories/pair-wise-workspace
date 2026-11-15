package relay

import (
	"container/heap"
	"context"
	"fmt"
	"hash/crc32"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"pairwise.local/relayctl/internal/impair"
	"pairwise.local/relayctl/internal/signal"
)

type packetAt struct {
	packet *rtp.Packet
	at     time.Time
}

type egress struct {
	*basePeer
	engine    *Engine
	room      string
	endpoint  string
	ssrc      uint32
	track     *webrtc.TrackLocalStaticRTP
	input     chan packetAt
	injector  *impair.Injector
	tracker   *Tracker
	buffer    *ReorderBuffer
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	flushCh   chan chan struct{}
	seq       sequenceState
	forwarded atomic.Uint64
	dropped   atomic.Uint64
	failed    atomic.Bool
}

func newEgress(engine *Engine, room, endpoint string, index int) (*egress, error) {
	pc, err := engine.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("new egress peer connection: %w", err)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "audio", endpoint)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("new egress track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add egress track: %w", err)
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buffer); err != nil {
				return
			}
		}
	}()
	cfg := engine.cfg.Impair
	cfg.Seed += int64(index+1) * 1009
	injector, err := impair.New(cfg)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(engine.ctx)
	peer := &egress{
		basePeer: newBasePeer(pc), engine: engine, room: room, endpoint: endpoint,
		ssrc: crc32.ChecksumIEEE([]byte(endpoint)), track: track,
		input: make(chan packetAt, 256), injector: injector,
		tracker: NewTracker(48000), buffer: NewReorderBuffer(engine.cfg.ReorderWindow, engine.cfg.ReorderHold),
		ctx: ctx, cancel: cancel, done: make(chan struct{}), flushCh: make(chan chan struct{}),
		seq: sequenceState{next: uint16(crc32.ChecksumIEEE([]byte(room + endpoint)))},
	}
	go peer.run()
	return peer, nil
}

func (p *egress) CreateOffer() (string, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create egress offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set egress offer: %w", err)
	}
	return offer.SDP, nil
}

func (p *egress) AcceptOffer(string) (string, error) { return "", notSupported("AcceptOffer") }

func (p *egress) AcceptAnswer(answer string) error {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return fmt.Errorf("set egress answer: %w", err)
	}
	return nil
}

func (p *egress) enqueue(packet *rtp.Packet, now time.Time) {
	if p.failed.Load() || p.ctx.Err() != nil {
		return
	}
	select {
	case p.input <- packetAt{packet: packet, at: now}:
	default:
		p.dropped.Add(1)
	}
}

func (p *egress) snapshot() EgressStats {
	dropped, duplicated := p.injector.Snapshot()
	late, _, _ := p.buffer.Snapshot()
	return EgressStats{
		Endpoint: p.endpoint, SSRC: p.ssrc,
		Forwarded: p.forwarded.Load(), Dropped: dropped + duplicated + p.dropped.Load(),
		Late: late, JitterMS: p.tracker.Stats(), Failed: p.failed.Load(),
	}
}

func (p *egress) run() {
	defer close(p.done)
	queue := &deliveryHeap{}
	heap.Init(queue)
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	reset := func() {
		stopTimer(timer)
		if queue.Len() > 0 {
			timer.Reset(time.Until((*queue)[0].Due))
		}
	}
	for {
		select {
		case <-p.ctx.Done():
			p.flush(time.Now().Add(time.Hour))
			return
		case item := <-p.input:
			for _, delivery := range p.injector.Inject(item.packet, item.at) {
				heap.Push(queue, delivery)
			}
			reset()
		case <-timer.C:
			now := time.Now()
			for queue.Len() > 0 && !(*queue)[0].Due.After(now) {
				p.release(heap.Pop(queue).(impair.Delivery))
			}
			reset()
		case ack := <-p.flushCh:
			p.flush(time.Now().Add(time.Hour))
			close(ack)
		}
	}
}

func (p *egress) release(delivery impair.Delivery) {
	p.tracker.Observe(delivery.Packet, delivery.Arrival)
	p.writeFrames(p.buffer.Push(delivery.Packet, delivery.Arrival))
}

func (p *egress) flush(now time.Time) {
	p.writeFrames(p.buffer.Flush(now))
}

func (p *egress) writeFrames(frames []Frame) {
	if p.failed.Load() {
		return
	}
	for index, frame := range frames {
		packet := impair.Clone(frame.Packet)
		packet.SSRC = p.ssrc
		p.seq.apply(packet)
		if err := writeWithRetry(p.engine.cfg.WriteBackoff, p.track, packet); err != nil {
			p.failed.Store(true)
			p.dropped.Add(uint64(len(frames) - index))
			p.cancel()
			return
		}
		p.forwarded.Add(1)
	}
}

func (p *egress) Close() error {
	p.cancel()
	<-p.done
	return p.basePeer.Close()
}

type sequenceState struct{ next uint16 }

func (s *sequenceState) apply(packet *rtp.Packet) {
	packet.SequenceNumber = s.next
	s.next++
}

type rtpWriter interface{ WriteRTP(*rtp.Packet) error }

func writeWithRetry(backoff time.Duration, writer rtpWriter, packet *rtp.Packet) error {
	var err error
	for attempt := 0; attempt <= 3; attempt++ {
		if err = writer.WriteRTP(packet); err == nil {
			return nil
		}
		if attempt < 3 {
			time.Sleep(backoff * time.Duration(1<<attempt))
		}
	}
	return err
}

type deliveryHeap []impair.Delivery

func (h deliveryHeap) Len() int { return len(h) }
func (h deliveryHeap) Less(i, j int) bool {
	if h[i].Due.Equal(h[j].Due) {
		if h[i].Arrival.Equal(h[j].Arrival) {
			return h[i].Packet.SequenceNumber < h[j].Packet.SequenceNumber
		}
		return h[i].Arrival.Before(h[j].Arrival)
	}
	return h[i].Due.Before(h[j].Due)
}
func (h deliveryHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *deliveryHeap) Push(value any) { *h = append(*h, value.(impair.Delivery)) }
func (h *deliveryHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

var timeNow = time.Now
var _ signal.Peer = (*egress)(nil)
