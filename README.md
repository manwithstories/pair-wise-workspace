# rtprelay loopback self-test

`rtprelay` is a terminal-only integration harness for a commentary return path.
It starts an in-process WebSocket signaling server, two Pion peers, synthetic
RTP generation, RTP relay, and RFC 3550 statistics. All media and signaling stay
on loopback interfaces; no external service, browser, camera, or microphone is
used.

## Requirements

- Go 1.22 or newer
- macOS or Linux on amd64 or arm64
- Pure Go build (`CGO_ENABLED=0` is supported)

## Commands

```sh
go build ./...
go test -race ./...

go run ./cmd/rtprelay selftest \
  --duration 4s --pps 200 --seed 7 --jitter-us 800 \
  --out out/report.json

go run ./cmd/rtprelay stats \
  --in out/packets.jsonl --out out/recheck.json

go run ./cmd/rtprelay signal --duration 2s --out out/signal.json
```

Fault injection accepts comma-separated probabilities:

```sh
go run ./cmd/rtprelay selftest \
  --duration 3s --pps 100 --fault drop=0.05 --seed 7 \
  --out out/drop.json
```

Use `--inject-candidate 10.0.0.5:40000` to verify rejection of a non-loopback
candidate. `--max-jitter-ms` fails the run when the measured maximum exceeds the
given value.

The program itself exits with:

- `0`: pass, including a graceful SIGINT with a partial report
- `2`: usage or input file error
- `3`: signaling failure
- `4`: candidate rejection or peer connection failure
- `5`: metric limit exceeded

When invoked through `go run`, the Go tool reports any non-zero child status as
shell status 1; run the built binary when checking the exact exit code.

## Output

The main report contains candidate filtering and pair results, selected loopback
pair, packet/forwarding counters, injected fault counters, and jitter summaries
(`mean_ms`, `p50_ms`, `p95_ms`, `max_ms`). The forwarder writes one JSONL record
per received packet to `packets.jsonl` next to the report; `stats` recomputes
the same jitter fields from this log.

Runtime reports, packet logs, and local binaries are generated under `out/` or
the repository root and are ignored by Git.
