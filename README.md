# avpipe

Interactive audio/video pipeline demo: PCM/WAV ingest, cross-block resampling,
channel-layout downmix, loudness control, a bounded queue with real backpressure,
and a reviewable audit index.

## Requirements

- Go 1.25.6 (macOS arm64/x86_64)
- No network access, no external services

## Build and test

    go mod tidy && go mod vendor
    go build -mod=vendor -o /tmp/avpipe ./cmd/avpipe
    go test -mod=vendor ./...

## Demo

    /tmp/avpipe demo --seed 42 --queue 4 --consumer-stall-ms 30 \
        --corrupt-tail-bytes 7 --verify-repeat --out /tmp/avpipe-demo

Outputs, in order: roundtrip_match, keyframe_hit, segment_count and per-segment
64-bit segment_sha256, queue_capacity, queue_high_water, backpressure_waits,
recovered_dropped_bytes, repeat_sha_match, plus loudness/peak and a PASS/FAIL
summary. Exit codes: 0 pass, 2 parameter/graph error, 3 strict input
corruption, 4 I/O, cancellation or internal failure.

    /tmp/avpipe check /tmp/avpipe-demo/fixtures/truncated.wav   # exits 3

## Layout

    cmd/avpipe            CLI: demo / check / version
    internal/media        rational clock, block, error model, PCM16 quantize
    internal/timeline     tracks, keyframe anchors, gap/dup/conflict rules
    internal/graph        dependency validation, stable topological order
    internal/wav          chunked RIFF PCM 16/24-bit reader, 16-bit recorder
    internal/resample     32-tap windowed-sinc FIR, phase-continuous across blocks
    internal/layout       mono/stereo/5.1 channel matrix
    internal/loudness     200 ms loudness window, -1 dBFS peak ceiling
    internal/stream       bounded queue and pipeline runner (cancel/recover)
    internal/audit        keyframe index, segment hashes, quantized summary
    internal/demo         generated fixtures, wiring and acceptance report

## Notes

- All test fixtures are generated from a seed; nothing external is read.
- Block boundaries use one exact rational division over cumulative positions, so
  they cannot drift.
- Cancellation unblocks the producer, then the consumer drains every block that
  was already enqueued; no queue was ever resized.
