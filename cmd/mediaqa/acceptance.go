package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pion/rtp"

	"mediaqa/internal/fixture"
	"mediaqa/internal/manifest"
	"mediaqa/internal/media"
	mp4scan "mediaqa/internal/mp4"
	"mediaqa/internal/pipeline"
	"mediaqa/internal/repair"
	"mediaqa/internal/report"
)

// acceptanceCmd generates normal, damaged and RTP-recovery assets from
// the seed, runs the full chain, and prints the acceptance metrics.
func acceptanceCmd(args []string) int {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	work := fs.String("work", filepath.Join(os.TempDir(), "mediaqa-demo"), "work directory")
	seed := fs.Int64("seed", 17, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return exitBadManifest
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		return exitUnrecoverable
	}
	outDir := filepath.Join(*work, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		return exitUnrecoverable
	}

	if err := genAssets(*work, uint32(*seed)); err != nil {
		fmt.Fprintln(os.Stderr, "acceptance: generate assets:", err)
		return exitUnrecoverable
	}
	m, err := manifest.Parse(strings.NewReader(acceptanceManifest))
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		return exitBadManifest
	}
	res, err := pipeline.Run(m, pipeline.Config{WorkDir: *work, OutDir: outDir, Workers: 4})
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		return exitBadManifest
	}

	// RTP recovery stage: reordered, duplicated, burst-lost packets with
	// wrapping sequence numbers and timestamps, recovered via cache.
	var errs media.List
	rtpRes := pipeline.RecoverRTP("rtp-video", 90000, rtpReceived(*seed), rtpCache(*seed), &errs)
	repairedRTP := repairRTPStamps(rtpRes, &errs)
	for _, e := range errs.All() {
		res.Errs.Add(e)
	}

	// Metrics.
	var sum report.Summary
	var v, a *pipeline.TrackResult
	indexParts := [][]byte{}
	for i := range res.Tracks {
		tr := &res.Tracks[i]
		sum.RepairedSamples += tr.RepairedCount
		sum.InvalidSamples += tr.InvalidCount
		if tr.IndexBytes != nil {
			indexParts = append(indexParts, tr.IndexBytes)
		}
		switch tr.TrackID {
		case "v1":
			v = tr
		case "a1":
			a = tr
		}
	}
	sum.RepairedSamples += repairedRTP
	if v != nil && a != nil {
		sum.AVSyncMaxMS = pipeline.AVSyncMaxMS(*v, *a)
	}
	sum.PacketRecoveryPct = rtpRes.Stats.RecoveryPct()
	if v != nil && v.Index != nil {
		sum.SeekP95MS = seekP95(v, *seed)
	}
	sum.IndexSHA256 = report.SHA256Hex(indexParts...)

	printErrors(&res.Errs)
	fmt.Printf("av_sync_max_ms: %.3f\n", sum.AVSyncMaxMS)
	fmt.Printf("packet_recovery_pct: %.2f\n", sum.PacketRecoveryPct)
	fmt.Printf("seek_p95_ms: %.3f\n", sum.SeekP95MS)
	fmt.Printf("repaired_samples: %d\n", sum.RepairedSamples)
	fmt.Printf("invalid_samples: %d\n", sum.InvalidSamples)
	fmt.Printf("index_sha256: %s\n", sum.IndexSHA256)

	if res.Errs.HasFatal() {
		return exitUnrecoverable
	}
	if v := sum.Exceeds(report.DefaultThresholds); len(v) > 0 {
		for _, msg := range v {
			fmt.Fprintln(os.Stderr, "threshold:", msg)
		}
		return exitMetrics
	}
	return exitOK
}

// acceptanceManifest references the generated assets: a normal track
// pair with non-zero-start edits, and a damaged (truncated) track.
const acceptanceManifest = `{"track_id":"v1","path":"normal.mp4","kind":"video","timescale":90000,"edits":[{"media_start":15000,"duration":255000}]}
{"track_id":"a1","path":"normal.mp4","kind":"audio","timescale":48000,"depends_on":["v1"],"edits":[{"media_start":8000,"duration":136000}]}
{"track_id":"v2","path":"damaged.mp4","kind":"video","timescale":90000}
`

// genAssets writes the normal and damaged MP4 fixtures deterministically.
func genAssets(dir string, seed uint32) error {
	var video []fixture.SampleSpec
	for i := 0; i < 90; i++ {
		video = append(video, fixture.SampleSpec{
			Duration: 3000,
			Key:      i%15 == 0, // sparse keyframes
			CTOffset: ctoPattern(i),
		})
	}
	var audio []fixture.SampleSpec
	for i := 0; i < 90; i++ {
		audio = append(audio, fixture.SampleSpec{Duration: 1600})
	}
	normal, err := fixture.Gen(seed, []fixture.TrackSpec{
		{ID: 1, Kind: "video", Timescale: 90000, Samples: video},
		{ID: 2, Kind: "audio", Timescale: 48000, Samples: audio},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "normal.mp4"), normal, 0o644); err != nil {
		return err
	}
	damaged, err := fixture.Gen(seed+1, []fixture.TrackSpec{
		{ID: 1, Kind: "video", Timescale: 90000, Samples: video},
	})
	if err != nil {
		return err
	}
	// Truncated mdat: tail samples become corrupt, complete ones survive.
	damaged = fixture.Truncate(damaged, len(damaged)-137)
	return os.WriteFile(filepath.Join(dir, "damaged.mp4"), damaged, 0o644)
}

// ctoPattern gives B-frame-like composition offsets.
func ctoPattern(i int) int32 {
	switch i % 3 {
	case 0:
		return 6000
	case 1:
		return -3000
	}
	return 0
}

// rtpStream builds the deterministic packet sequence for the seed: one
// source stream, then a "wire" view (reordered, duplicated, burst-lost)
// and a retransmission cache holding every lost packet.
func rtpStream(seed int64) (received, cache []*rtp.Packet) {
	const n = 200
	var src []*rtp.Packet
	for i := 0; i < n; i++ {
		src = append(src, &rtp.Packet{Header: rtp.Header{
			// Sequence numbers and timestamps both wrap.
			SequenceNumber: uint16(65500 + i),
			Timestamp:      uint32(1<<32-45000) + uint32(i)*3000,
		}, Payload: []byte{byte(i)}})
	}
	// One packet carries a regressive timestamp; the repair stage must
	// reassign it without disturbing decode order.
	src[150].Timestamp -= 90000
	dropped := map[int]bool{}
	for i := 0; i < n; i++ {
		if i%37 == 3 || (i >= 100 && i < 104) { // scattered + burst loss
			dropped[i] = true
			cache = append(cache, src[i])
		}
	}
	for i := 0; i < n; i++ {
		if dropped[i] {
			continue
		}
		if i%10 == 4 && i+1 < n && !dropped[i+1] { // swap: out-of-order pair
			received = append(received, src[i+1], src[i])
			i++
			continue
		}
		received = append(received, src[i])
		if i%41 == 5 {
			received = append(received, src[i]) // duplicate
		}
	}
	return received, cache
}

var rtpRecv, rtpCacheStore []*rtp.Packet
var rtpSeed int64 = -1

func rtpReceived(seed int64) []*rtp.Packet {
	if rtpSeed != seed {
		rtpRecv, rtpCacheStore = rtpStream(seed)
		rtpSeed = seed
	}
	return rtpRecv
}

func rtpCache(seed int64) []*rtp.Packet {
	rtpReceived(seed)
	return rtpCacheStore
}

// repairRTPStamps converts delivered RTP packets to sample stamps and
// runs timestamp repair over them, returning the repaired count.
func repairRTPStamps(res pipeline.RTPResult, errs *media.List) int {
	var samples []mp4scan.Sample
	for i, p := range res.Delivered {
		samples = append(samples, mp4scan.Sample{
			Index:    i,
			DTS:      p.Timestamp.Ticks(90000),
			PTS:      p.Timestamp.Ticks(90000),
			Duration: 3000,
		})
	}
	before := errs.Count(media.SeverityRecoverable)
	repair.Stamps(samples, 90000, "rtp-video", errs)
	return errs.Count(media.SeverityRecoverable) - before
}

// seekP95 measures random-access seek latency against the built index.
func seekP95(v *pipeline.TrackResult, seed int64) float64 {
	rng := rand.New(rand.NewSource(seed))
	var maxPres int64
	for _, e := range v.Index.Entries {
		if e.PresTicks > maxPres {
			maxPres = e.PresTicks
		}
	}
	if maxPres <= 0 {
		return 0
	}
	const seeks = 200
	durs := make([]float64, 0, seeks)
	for i := 0; i < seeks; i++ {
		target := rng.Int63n(maxPres)
		start := time.Now()
		if _, ok := v.Index.Seek(target); !ok {
			continue
		}
		durs = append(durs, float64(time.Since(start).Nanoseconds())/1e6)
	}
	return report.Percentile(durs, 95)
}
