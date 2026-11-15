package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	rtpv1 "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"relayd/internal/forward"
	"relayd/internal/peer"
	"relayd/internal/report"
	"relayd/internal/signal"
)

// cmdForward runs the relay node: it accepts one ingress offer and
// --egress sink offers via the signaling server, then fans packets out.
func cmdForward(args []string) int {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9000", "loopback signaling listen address")
	egressN := fs.Int("egress", 3, "number of egress legs to wait for")
	queue := fs.Int("queue", 512, "per-egress queue size")
	session := fs.String("session", "relay", "signaling session name")
	out := fs.String("out", "", "write report JSON to this file on exit")
	if err := fs.Parse(args); err != nil {
		return exitSignal
	}

	srv, err := signal.Listen(*listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	ctx, stop := osSignalContext()
	defer stop()
	go srv.Serve(ctx)
	fmt.Fprintf(os.Stderr, "forward: signaling on %s, waiting for 1 source + %d sinks\n",
		srv.Addr(), *egressN)

	relay, err := signal.Dial(srv.Addr(), *session, "relay", "relay")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	defer relay.Close()

	api := peer.NewAPI()
	fwd := forward.New(*queue)
	est := forward.NewJitterEstimator(1 << 20)
	var pairs int

	// Ingress: first offer wins.
	ingress, code := acceptLeg(ctx, relay, api, nil)
	if code != exitOK {
		return code
	}
	ingress.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		clock := int64(track.Codec().ClockRate)
		buf := make([]byte, 1600)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			var p rtpv1.Packet
			if err := p.Unmarshal(buf[:n]); err != nil {
				continue
			}
			now := time.Now().UnixNano()
			est.Add(int64(p.Timestamp)*1e9/clock, now)
			fwd.Forward(buf[:n])
		}
	})

	// Egress legs.
	for i := 0; i < *egressN; i++ {
		track, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "relayd")
		if err != nil {
			return exitICE
		}
		leg, code := acceptLeg(ctx, relay, api, track)
		if code != exitOK {
			return code
		}
		pairs += peer.Pairs(leg.filter.Accepted(), leg.peerFilter.Accepted())
		fwd.AddEgress(0x90000, func(buf []byte) error {
			var p rtpv1.Packet
			if err := p.Unmarshal(buf); err != nil {
				return err
			}
			return track.WriteRTP(&p)
		})
		fmt.Fprintf(os.Stderr, "forward: egress leg %d connected\n", i)
	}
	fmt.Fprintln(os.Stderr, "forward: relaying; press Ctrl-C to stop")

	<-ctx.Done()
	fwd.Close()
	forwarded, dropped, egressErrs := fwd.Stats()
	st := forward.StatsOf(est.Samples())
	rep := report.Report{
		CandidatePairs:   pairs,
		ForwardedPackets: forwarded,
		DroppedPackets:   dropped,
		JitterMs:         report.Jitter{Mean: st.Mean, Max: st.Max, P99: st.P99, Samples: st.Samples},
		Egress:           *egressN,
	}
	if egressErrs > 0 {
		fmt.Fprintf(os.Stderr, "forward: %d egress errors\n", egressErrs)
	}
	if *out != "" {
		if err := report.WriteFile(*out, rep); err != nil {
			fmt.Fprintln(os.Stderr, "forward: write report:", err)
		}
	}
	b, _ := report.Encode(rep)
	fmt.Println(string(b))
	return exitOK
}

type acceptedLeg struct {
	pc         *webrtc.PeerConnection
	filter     *peer.CandidateFilter
	peerFilter *peer.CandidateFilter
}

// acceptLeg waits for one offer on the signaling client, answers it and
// waits for ICE connectivity.
func acceptLeg(ctx context.Context, relay *signal.Client, api *webrtc.API,
	track *webrtc.TrackLocalStaticRTP) (*acceptedLeg, int) {
	from, sdp, err := relay.AwaitOffer(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forward: await offer:", err)
		return nil, exitSignal
	}
	pc, err := peer.NewConnection(api)
	if err != nil {
		return nil, exitICE
	}
	leg := &acceptedLeg{pc: pc, filter: peer.NewCandidateFilter(), peerFilter: peer.NewCandidateFilter()}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			leg.filter.Add(c.ToJSON().Candidate)
		}
	})
	if track != nil {
		if _, err := pc.AddTrack(track); err != nil {
			return nil, exitICE
		}
	}
	if err := pc.SetRemoteDescription(
		webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return nil, exitICE
	}
	for _, c := range candidatesFromSDP(sdp) {
		leg.peerFilter.Add(c)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, exitICE
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return nil, exitICE
	}
	if err := relay.Answer(from, peer.GatheredSDP(pc)); err != nil {
		fmt.Fprintln(os.Stderr, "forward: answer:", err)
		return nil, exitSignal
	}
	if err := peer.WaitConnected(pc, 15*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, exitICE
	}
	return leg, exitOK
}

// candidatesFromSDP extracts a=candidate lines from an SDP blob.
func candidatesFromSDP(sdp string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(sdp); i++ {
		if i == len(sdp) || sdp[i] == '\n' {
			line := sdp[start:i]
			start = i + 1
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 11 && line[:11] == "a=candidate" {
				out = append(out, line[2:])
			}
		}
	}
	return out
}
