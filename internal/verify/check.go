package verify

import (
	"relayaudit/internal/clock"
	"relayaudit/internal/inspect"
	"relayaudit/internal/proxy"
	"relayaudit/internal/timeline"
	"relayaudit/internal/transform"
	"relayaudit/internal/wire"
)

// Check validates the repaired output against the source scan, the injected
// damage and a repeat run's digest.
func Check(
	out []byte,
	scan *wire.ScanResult,
	res *timeline.Resolution,
	ranges []transform.Range,
	repeatDigest string,
	exp Expected,
) *Result {
	r := &Result{
		DamageRanges:     len(ranges),
		Ranges:           ranges,
		OutputSHA256:     SHA256Hex(out),
		KeyIntervalTicks: map[uint8]uint64{},
		ExitCode:         0,
	}
	r.Kept, r.Dropped = countKept(scan, ranges)

	// Independently re-scan the output: it must have a trusted index and no
	// damage of its own.
	outScan, err := wire.Scan(out)
	if err != nil {
		r.fail("repaired output does not parse: %v", err)
		r.ExitCode = exitCodeFor(r.Problems)
		return r
	}
	if outScan.IndexRebuilt {
		r.fail("repaired output index was not trusted")
	}
	if len(outScan.Damage) != 0 {
		r.fail("repaired output still contains %d damaged spans", len(outScan.Damage))
	}
	if int(outScan.Header.RecordCount) != r.Kept {
		r.fail("repaired header declares %d records, %d survived", outScan.Header.RecordCount, r.Kept)
	}
	if int(outScan.Header.Tracks) != exp.TrackCount {
		r.fail("repaired output declares %d tracks, want %d", outScan.Header.Tracks, exp.TrackCount)
	}
	for _, rec := range outScan.Records {
		if !rec.ValidCRC() {
			r.fail("repaired record at output offset %d fails CRC", rec.Offset)
			break
		}
	}

	// Damage localisation: the first CRC/truncation span must start exactly
	// where the fixture injected damage.
	r.measureLocalisation(scan, ranges, exp)
	r.IndexRebuilt = boolToInt(scan.IndexRebuilt)

	// Repeat run consistency.
	if repeatDigest != "" {
		if repeatDigest == r.OutputSHA256 {
			r.RepeatSHA256Equal = 1
		} else {
			r.fail("repeat run digest %s differs from %s", repeatDigest, r.OutputSHA256)
		}
	}

	// Frame structure and keyframe cadence of the surviving records.
	sc := inspect.NewScanner()
	for _, rec := range outScan.Records {
		sc.Push(rec.Track, rec.PTS, rec.Payload, rec.Offset)
	}
	stats := sc.Summarise()
	r.Frames = stats.Frames
	r.Tracks = stats.Tracks
	for t, v := range stats.KeyIntervalAvg {
		r.KeyIntervalTicks[t] = v
	}
	for _, t := range []uint8{1, 2} {
		if avg, ok := stats.KeyIntervalAvg[t]; ok && avg != 2*clock.Rate {
			r.fail("track %d mean keyframe interval %d ticks, want %d", t, avg, 2*clock.Rate)
		}
	}

	// Proxy frames are hashed independently of the pipeline.
	ticks := proxyTicks(res)
	ps := proxy.Build(73, ticks, proxy.DefaultSize)
	r.ProxyHash = ps.CombinedHash()

	if exp.SourceSize > 0 && scan.SourceSize != exp.SourceSize {
		r.fail("source size %d, expected %d", scan.SourceSize, exp.SourceSize)
	}
	r.ExitCode = exitCodeFor(r.Problems)
	return r
}
