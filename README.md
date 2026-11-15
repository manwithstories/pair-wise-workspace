# relayctl

Loopback-only one-to-many RTP relay and end-to-end verification harness for
multiplayer voice. The binary generates deterministic Opus-like RTP frames and
does not access a microphone, camera, browser, or external service.

## Build and verify

```sh
go build ./...
go run ./cmd/relayctl verify \
  --signal 127.0.0.1:47000 \
  --duration 12s \
  --egress 3 \
  --impair delay=20ms,reorder=5,loss=0.01,seed=7 \
  --max-p95-jitter 15 \
  --out out/report.json
go run ./cmd/relayctl report --input out/report.json --format table
```

`verify` starts an in-process signaling server, one real Pion publisher, one
real Pion subscriber per egress, and waits for all ICE candidate pairs before
sending media. It emits exactly one JSON line on stdout after the run. Logs and
the verdict go to stderr. The JSON artifact is ignored by git and must not be
included in delivery.

## Long-running service

```sh
go run ./cmd/relayctl serve --listen 127.0.0.1:47000 \
  --impair delay=20ms,reorder=5,loss=0.01,seed=7

# In separate terminals:
go run ./cmd/relayctl subscribe --signal 127.0.0.1:47000 --room demo --duration 10s
go run ./cmd/relayctl publish --signal 127.0.0.1:47000 --room demo --duration 10s
```

Subscribers can join and leave while the publisher remains connected. The
ingress PeerConnection is never rebuilt for egress churn. Each egress has its
own SSRC rewrite state and continuous, wrapping sequence counter.

## Signal protocol

Signaling is newline-delimited JSON over TCP. Every sender uses a strictly
increasing `seq`; duplicate or regressed sequences are rejected and counted.

```json
{"type":"join","seq":1,"room":"demo","role":"subscriber"}
{"type":"joined","seq":1,"endpoint":"s-2","room":"demo","role":"subscriber"}
{"type":"offer","seq":2,"sdp":"v=0\r\n..."}
{"type":"answer","seq":2,"sdp":"v=0\r\n..."}
{"type":"candidate","seq":3,"candidate":{"candidate":"candidate:...","sdpMid":"0","sdpMLineIndex":0}}
```

The server allocates endpoint IDs, owns room state, forwards SDP/ICE to the
media engine, and times out a handshake after five seconds without progress.

## Data path

`media.Source` -> publisher PeerConnection -> `relay.Engine` ingress ->
per-egress impairment/reorder/jitter -> `TrackLocalStaticRTP` egress ->
subscriber PeerConnection. Impairment decisions and observed arrival times are
derived from packet counters, seed, and RTP timestamps, so the same arguments
produce identical packet and jitter statistics across runs.

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | Verification passed |
| 2 | Invalid argument or configuration |
| 3 | Non-loopback listener, SDP, candidate, or mDNS address |
| 4 | Signal/ICE/handshake timeout or media setup failure |
| 5 | p95 jitter, drop ratio, or failed egress threshold exceeded |

Only IP literals in `127.0.0.0/8` are accepted. Pion is configured for UDP4,
loopback candidates, and mDNS disabled.

## Dependency note

The requested `pion/webrtc/v4 v4.0.x` line requires `github.com/pion/rtp`
v1.8.x internally (v4.0.16 declares v1.8.13). Forcing v1.4.x breaks
`pion/interceptor` and `pion/srtp` compilation, so this repository pins the
compatible v1.8.13 with Go 1.22.

## Validation

```sh
go vet ./...
go test -race ./...
```
