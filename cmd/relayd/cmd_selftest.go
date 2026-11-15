package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	rtpv1 "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"relayd/internal/forward"
	"relayd/internal/loadgen"
	"relayd/internal/peer"
	"relayd/internal/report"
	"relayd/internal/signal"
)

const selftestSeed = 11766

type egressLeg struct {
	relayPC     *webrtc.PeerConnection
	sinkPC      *webrtc.PeerConnection
	relayFilter *peer.CandidateFilter
	sinkFilter  *peer.CandidateFilter
	received    atomic.Uint64
	est         *forward.JitterEstimator
	done        chan struct{}
}

func cmdSelftest(args []string) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	peers := fs.Int("peers", 3, "number of egress legs")
	packets := fs.Int("packets", 2000, "packets to send")
	rate := fs.Int("rate", 500, "packets per second")
	jitterMs := fs.Float64("jitter-ms", 8, "injected jitter amplitude in ms")
	out := fs.String("out", "", "write report JSON to this file")
	queue := fs.Int("queue", 1024, "per-egress queue size")
	if err := fs.Parse(args); err != nil {
		return exitStats
	}
	rep, code := runSelftest(*peers, *packets, *rate, *jitterMs, *queue)
	if *out != "" {
		if err := report.WriteFile(*out, rep); err != nil {
			fmt.Fprintln(os.Stderr, "selftest: write report:", err)
		}
	}
	b, _ := report.Encode(rep)
	if code == exitOK {
		fmt.Println(string(b))
	} else {
		fmt.Fprintln(os.Stderr, "selftest: failed:", string(b))
	}
	return code
}

func runSelftest(peersN, packetsN, rate int, jitterMs float64, queueSize int) (report.Report, int) {
	rep := report.Report{Egress: peersN}
	gen := loadgen.New(loadgen.Config{
		Packets: packetsN, Rate: rate, JitterMs: jitterMs,
		Seed: selftestSeed, SSRC: 0x11766, PayloadSize: 64,
	})

	srv, err := signal.Listen("127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return rep, exitSignal
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go srv.Serve(ctx)
	defer srv.Close()

	api := peer.NewAPI()
	fwd := forward.New(queueSize)

	// Ingress leg: source offers, relay answers.
	srcPC, relayIn, err := newPair(api)
	if err != nil {
		return rep, exitICE
	}
	srcTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "loadgen")
	if err != nil {
		return rep, exitICE
	}
	if _, err := srcPC.AddTrack(srcTrack); err != nil {
		return rep, exitICE
	}
	relayIn.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 1600)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			fwd.Forward(buf[:n])
		}
	})
	if code := connectViaSignal(srv.Addr(), "ingress", srcPC, relayIn, "src", "relay-in"); code != exitOK {
		return rep, code
	}

	// Egress legs: relay offers, sinks answer.
	legs := make([]*egressLeg, peersN)
	for i := 0; i < peersN; i++ {
		leg, err := newEgressLeg(api, fwd, gen, packetsN, i)
		if err != nil {
			return rep, exitICE
		}
		legs[i] = leg
		session := fmt.Sprintf("egress-%d", i)
		code := connectViaSignal(srv.Addr(), session, leg.relayPC, leg.sinkPC,
			fmt.Sprintf("relay-out-%d", i), fmt.Sprintf("sink-%d", i))
		if code != exitOK {
			return rep, code
		}
	}

	// Stream all packets on the modeled schedule.
	start := time.Now()
	for {
		pkt, ok := gen.Next()
		if !ok {
			break
		}
		if d := time.Until(start.Add(pkt.SendAt)); d > 0 {
			time.Sleep(d)
		}
		var p rtpv1.Packet
		if err := p.Unmarshal(pkt.Data); err == nil {
			_ = srcTrack.WriteRTP(&p)
		}
	}

	// Wait for every sink to drain, with a generous bound.
	deadline := time.Now().Add(time.Duration(packetsN/rate+15) * time.Second)
	for _, leg := range legs {
		select {
		case <-leg.done:
		case <-time.After(time.Until(deadline)):
			return finishReport(rep, fwd, legs), exitStats
		}
	}
	fwd.Close()

	rep = finishReport(rep, fwd, legs)
	for _, leg := range legs {
		if int(leg.received.Load()) != packetsN {
			return rep, exitStats
		}
	}
	if rep.ForwardedPackets != uint64(packetsN) {
		return rep, exitStats
	}
	if rep.CandidatePairs != 2*peersN {
		return rep, exitStats
	}
	return rep, exitOK
}

func newPair(api *webrtc.API) (a, b *webrtc.PeerConnection, err error) {
	a, err = peer.NewConnection(api)
	if err != nil {
		return nil, nil, err
	}
	b, err = peer.NewConnection(api)
	if err != nil {
		return nil, nil, err
	}
	return a, b, nil
}

func newEgressLeg(api *webrtc.API, fwd *forward.Forwarder, gen *loadgen.Generator,
	packetsN, idx int) (*egressLeg, error) {
	leg := &egressLeg{
		relayFilter: peer.NewCandidateFilter(),
		sinkFilter:  peer.NewCandidateFilter(),
		est:         forward.NewJitterEstimator(packetsN),
		done:        make(chan struct{}),
	}
	var err error
	leg.relayPC, leg.sinkPC, err = newPair(api)
	if err != nil {
		return nil, err
	}
	leg.relayPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			leg.relayFilter.Add(c.ToJSON().Candidate)
		}
	})
	leg.sinkPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			leg.sinkFilter.Add(c.ToJSON().Candidate)
		}
	})
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "relayd")
	if err != nil {
		return nil, err
	}
	if _, err := leg.relayPC.AddTrack(track); err != nil {
		return nil, err
	}
	fwd.AddEgress(0x90000, func(buf []byte) error {
		var p rtpv1.Packet
		if err := p.Unmarshal(buf); err != nil {
			return err
		}
		return track.WriteRTP(&p)
	})
	leg.sinkPC.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 1600)
		for {
			n, _, err := tr.Read(buf)
			if err != nil {
				return
			}
			var p rtpv1.Packet
			if err := p.Unmarshal(buf[:n]); err != nil || len(p.Payload) < 4 {
				continue
			}
			idx := int(binary.BigEndian.Uint32(p.Payload[:4]))
			send, recv := gen.Schedule(idx)
			leg.est.Add(int64(send), int64(recv))
			if int(leg.received.Add(1)) == packetsN {
				close(leg.done)
			}
		}
	})
	return leg, nil
}

// connectViaSignal exchanges gathering-complete SDPs between offerPC and
// answerPC through the signaling server, then waits for ICE.
func connectViaSignal(srvAddr, session string, offerPC, answerPC *webrtc.PeerConnection,
	offerID, answerID string) int {
	answerer, err := signal.Dial(srvAddr, session, answerID, "peer")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	defer answerer.Close()
	offerer, err := signal.Dial(srvAddr, session, offerID, "peer")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	defer offerer.Close()

	offer, err := offerPC.CreateOffer(nil)
	if err != nil {
		return exitICE
	}
	if err := offerPC.SetLocalDescription(offer); err != nil {
		return exitICE
	}
	sdpOffer := peer.GatheredSDP(offerPC)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		sdp string
		err error
	}
	answerCh := make(chan result, 1)
	go func() {
		sdp, err := offerer.Offer(ctx, answerID, sdpOffer, 2, 2*time.Second)
		answerCh <- result{sdp, err}
	}()

	from, sdp, err := answerer.AwaitOffer(ctx)
	if err != nil || from != offerID {
		fmt.Fprintln(os.Stderr, "selftest: await offer:", err)
		return exitSignal
	}
	if err := answerPC.SetRemoteDescription(
		webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return exitICE
	}
	answer, err := answerPC.CreateAnswer(nil)
	if err != nil {
		return exitICE
	}
	if err := answerPC.SetLocalDescription(answer); err != nil {
		return exitICE
	}
	if err := answerer.Answer(offerID, peer.GatheredSDP(answerPC)); err != nil {
		fmt.Fprintln(os.Stderr, "selftest: answer:", err)
		return exitSignal
	}
	res := <-answerCh
	if res.err != nil {
		fmt.Fprintln(os.Stderr, "selftest: offer:", res.err)
		return exitSignal
	}
	if err := offerPC.SetRemoteDescription(
		webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.sdp}); err != nil {
		return exitICE
	}
	if err := peer.WaitConnected(offerPC, 10*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitICE
	}
	if err := peer.WaitConnected(answerPC, 10*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitICE
	}
	return exitOK
}

func finishReport(rep report.Report, fwd *forward.Forwarder, legs []*egressLeg) report.Report {
	forwarded, dropped, _ := fwd.Stats()
	rep.ForwardedPackets = forwarded
	rep.DroppedPackets = dropped
	var samples []float64
	for _, leg := range legs {
		rep.CandidatePairs += peer.Pairs(leg.relayFilter.Accepted(), leg.sinkFilter.Accepted())
		samples = append(samples, leg.est.Samples()...)
	}
	st := forward.StatsOf(samples)
	rep.JitterMs = report.Jitter{Mean: st.Mean, Max: st.Max, P99: st.P99, Samples: st.Samples}
	return rep
}
