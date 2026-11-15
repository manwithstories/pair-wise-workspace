// Package peer builds PeerConnections pinned to loopback-only ICE.
package peer

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// NewAPI returns a WebRTC API that gathers UDPv4 host candidates on
// loopback interfaces only; every other address is filtered out.
func NewAPI() *webrtc.API {
	var se webrtc.SettingEngine
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return webrtc.NewAPI(webrtc.WithSettingEngine(se))
}

// NewConnection creates a bare PeerConnection (no ICE servers: host
// candidates on 127.0.0.1 are sufficient and keep everything offline).
func NewConnection(api *webrtc.API) (*webrtc.PeerConnection, error) {
	return api.NewPeerConnection(webrtc.Configuration{})
}

// WaitConnected blocks until the peer connection (including DTLS) reports
// connected or the timeout elapses.
func WaitConnected(pc *webrtc.PeerConnection, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch pc.ConnectionState() {
	case webrtc.PeerConnectionStateConnected:
		return nil
	case webrtc.PeerConnectionStateFailed:
		return fmt.Errorf("peer: ICE connection failed")
	}
	done := make(chan struct{})
	failed := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-done:
			default:
				close(done)
			}
		case webrtc.PeerConnectionStateFailed:
			select {
			case <-failed:
			default:
				close(failed)
			}
		}
	})
	select {
	case <-done:
		return nil
	case <-failed:
		return fmt.Errorf("peer: ICE connection failed")
	case <-ctx.Done():
		return fmt.Errorf("peer: ICE connect timeout after %s", timeout)
	}
}

// GatheredSDP returns the local description once ICE gathering completes.
func GatheredSDP(pc *webrtc.PeerConnection) string {
	<-webrtc.GatheringCompletePromise(pc)
	return pc.LocalDescription().SDP
}
