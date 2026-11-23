package pipeline

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/pion/rtp"

	"github.com/example/mediad/internal/audio"
	"github.com/example/mediad/internal/clock"
	"github.com/example/mediad/internal/ingest"
	"github.com/example/mediad/internal/model"
	rtppkg "github.com/example/mediad/internal/rtp"
	"github.com/example/mediad/internal/sync"
	"github.com/example/mediad/internal/txn"
	"github.com/example/mediad/internal/verify"
)

// Result bundles metrics, the manifest and quarantine records.
type Result struct {
	Metrics   Metrics
	Manifest  *verify.Manifest
	Rejected  []verify.SegmentRecord
	WorkDir   string
}

// Run executes one full, self-contained delivery pipeline.
func Run(opts Options) (*Result, error) {
	if opts.VideoSamples == 0 {
		opts.VideoSamples = 12
	}
	work, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	if err := resetManagedDirs(work); err != nil {
		return nil, err
	}

	fixtures, err := generateFixtures(work, opts.VideoSamples)
	if err != nil {
		return nil, err
	}
	res := &Result{WorkDir: work, Manifest: &verify.Manifest{}}
	m := &res.Metrics

	// ---- Link 2: RTP jitter/recovery on a synthetic capture stream.
	rtpStats, err := runRTPStream(opts)
	if err != nil {
		return nil, err
	}
	m.RecoveredPackets = rtpStats.Recovered
	m.Unrecoverable = rtpStats.UnrecoverablePackets

	// ---- Link 1: container parse + conflict adjudication.
	sourceSum, ingestElapsed, err := parseGoodSource(fixtures)
	if err != nil {
		return nil, err
	}
	m.MetadataTimeRatio = float64(ingestElapsed) /
		float64(inputDurationMicros(opts.VideoSamples))

	// Damaged inputs must be quarantined, not abort the run.
	if rec, ok := quarantineIfFails(fixtures.DamagedMP4, "truncated-mp4"); ok {
		res.Rejected = append(res.Rejected, rec)
	}
	if rec, ok := quarantineBadLayout(fixtures.BadLayoutMP4); ok {
		res.Rejected = append(res.Rejected, rec)
	}

	// ---- Link 3a: audio mapping validation.
	var audioTrack *ingest.AudioTrackLayout
	for _, tr := range sourceSum.Tracks {
		if tr.Kind == "audio" {
			audioTrack = tr.Audio
		}
	}
	mapping, err := audio.PlanMapping(audioTrack)
	if err != nil {
		return nil, err
	}
	if err := mapping.CheckResampleBoundary(nil); err != nil {
		return nil, err
	}

	// ---- Link 3b: A/V offset estimation and bounded alignment.
	offset, aligned, err := alignTracks()
	if err != nil {
		return nil, err
	}
	m.AVOffsetMicros = int64(offset)
	if err := sync.RejectOutOfTolerance(aligned); err != nil {
		return nil, err
	}

	// ---- Link 3c: segment plan through journal + committer.
	journalPath := filepath.Join(work, "txn")
	j, _, err := txn.OpenJournal(journalPath)
	if err != nil {
		return nil, err
	}
	crash := txn.CrashPoint(opts.CrashAfter)
	committer := txn.NewCommitter(work, j, crash)
	if err := committer.Prepare(); err != nil {
		return nil, err
	}

	planned := verify.PlannedKeyframes(uint64(opts.VideoSamples), 3)
	actual := videoKeyframes(sourceSum)
	m.KeyframeHitRate = verify.KeyframeHitRate(planned, actual)

	for i := uint32(0); i < opts.VideoSamples; i++ {
		segID := fmt.Sprintf("seg-%02d", i+1)
		payload := segmentPayload(segID, i, opts.Seed)
		if len(payload) > MaxSegmentBytes {
			return nil, model.New(model.KindUnsupported, "pipeline",
				"segment", "segment exceeds 256 MiB cap")
		}
		verifier := func(want string) error {
			if verify.HashBytes(payload) != want {
				return model.New(model.KindChecksum, "pipeline",
					"verify", segID+": pre-commit hash mismatch")
			}
			return nil
		}
		cr, err := committer.Run(segID, payload, verifier)
		if err != nil {
			if model.IsRetryable(err) {
				// Simulated interruption: recover, then resume the run.
				_ = j.Close()
				rollbacks, rerun, newJournal, err2 := recoverAndResume(work,
					journalPath, segID, payload, verifier)
				if err2 != nil {
					return nil, err2
				}
				j = newJournal
				committer.SetJournal(j)
				committer.ClearCrash()
				committer.RollbackCount += rollbacks
				cr = rerun
			} else {
				return nil, err
			}
		}
		res.Manifest.Segments = append(res.Manifest.Segments,
			verify.SegmentRecord{
				ID: segID, Path: cr.OutputPath, SHA256: cr.Hash,
				Size: int64(len(payload)),
				KeyframeCount: countKeyframesForSegment(i, actual),
				SampleCount:   1, TrackID: 1,
			})
	}
	m.RollbackCount = committer.RollbackCount
	m.RejectedSegments = len(res.Rejected)
	if err := j.Close(); err != nil {
		return nil, err
	}

	// ---- Final independent verification.
	matched, err := verify.RecomputeHashes(res.Manifest)
	if err != nil {
		return nil, err
	}
	m.HashMatched = matched
	m.HashTotal = len(res.Manifest.Segments)
	m.Segments = m.HashTotal

	exp := verify.RoundTripExpectation{
		TrackID: 1, SampleCount: uint64(opts.VideoSamples),
		Width: 1920, Height: 1080, Keyframes: planned,
	}
	roundOK := 0
	for i := 0; i < int(opts.VideoSamples); i++ {
		if err := verify.RoundTripMP4(fixtures.GoodMP4, exp); err == nil {
			roundOK++
		}
	}
	m.RoundTripRate = float64(roundOK) / float64(opts.VideoSamples)

	if err := writeManifest(work, res); err != nil {
		return nil, err
	}
	return res, nil
}

// runRTPStream drives the jitter buffer with disorder, duplicates,
// timestamp wraparound and a recoverable burst of `loss` packets.
func runRTPStream(opts Options) (rtppkg.Stats, error) {
	buf, err := rtppkg.NewBuffer(rtppkg.Config{
		ClockRate: 90000, WindowSize: 16,
		SegmentOf: func(seq uint16) string {
			return fmt.Sprintf("seg-%02d", seq/4+1)
		},
	})
	if err != nil {
		return rtppkg.Stats{}, err
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	const total = 40
	withheld := map[uint32]bool{}
	for i := 0; i < opts.Loss && i < total-8; i++ {
		withheld[uint32(1+i)] = true
	}
	push := func(seq uint32, ts uint32) error {
		return buf.Push(&rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: uint16(seq), Timestamp: ts,
			},
			Payload: []byte{byte(seq)},
		})
	}
	baseTS := uint32(0xFFFFFFF0) // force 32-bit wraparound
	retransmitted := map[uint32]bool{}
	for seq := uint32(0); seq < total; seq++ {
		if withheld[seq] {
			continue
		}
		if err := push(seq, baseTS+seq*3000); err != nil {
			return rtppkg.Stats{}, err
		}
		// Deterministic small reordering between neighbors, but never
		// for packets that are being withheld for the loss test.
		if seq > 4 && rng.Intn(3) == 0 && seq+1 < total &&
			!withheld[seq+1] {
			if err := push(seq+1, baseTS+(seq+1)*3000); err != nil {
				return rtppkg.Stats{}, err
			}
			seq++
		}
		// Once the stream has advanced a few packets past the burst,
		// retransmit the withheld packets while their windows are open.
		if seq == uint32(5+opts.Loss) {
			for lost := range withheld {
				if retransmitted[lost] {
					continue
				}
				if err := push(lost, baseTS+lost*3000); err != nil {
					return rtppkg.Stats{}, err
				}
				retransmitted[lost] = true
			}
		}
	}
	// Any still-unretransmitted packets arrive late (beyond window).
	for seq := range withheld {
		if retransmitted[seq] {
			continue
		}
		if err := push(seq, baseTS+seq*3000); err != nil {
			return rtppkg.Stats{}, err
		}
	}
	// One duplicate to exercise de-duplication.
	_ = push(0, baseTS)
	buf.Flush()
	return buf.Stats(), nil
}

func parseGoodSource(fixtures FixtureSet) (*ingest.Summary, time.Duration, error) {
	start := time.Now()
	f, err := os.Open(fixtures.GoodMP4)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sum, err := ingest.ParseMP4(f, fixtures.GoodMP4)
	if err != nil {
		return nil, 0, err
	}
	if err := ingest.Adjudicate(sum, ingest.DefaultPolicy()); err != nil {
		return nil, 0, err
	}
	elapsed := time.Since(start)
	return sum, elapsed, nil
}

func alignTracks() (clock.Micros, []sync.AlignedUnit, error) {
	var v, a []sync.Point
	for i := int64(0); i < 12; i++ {
		t := i * 33333
		v = append(v, sync.Point{TrackID: 1, Time: clock.Micros(t)})
		// Audio is captured a deterministic 15 ms early relative to video.
		a = append(a, sync.Point{TrackID: 2, IsAudio: true,
			Time: clock.Micros(t + 15000)})
	}
	offset, err := sync.Estimate(v, a)
	if err != nil {
		return 0, nil, err
	}
	units := append(v, a...)
	aligned := sync.Align(units, offset, sync.DefaultConfig())
	return offset, aligned, nil
}

func videoKeyframes(sum *ingest.Summary) []uint32 {
	for _, tr := range sum.Tracks {
		if tr.Kind == "video" {
			return tr.Video.KeyframeIndexes
		}
	}
	return nil
}

func countKeyframesForSegment(i uint32, all []uint32) int {
	first := i*3 + 1
	last := first + 2
	n := 0
	for _, k := range all {
		if k >= first && k <= last {
			n++
		}
	}
	return n
}

func inputDurationMicros(samples uint32) time.Duration {
	return time.Duration(samples) * 33333 * time.Microsecond
}

func segmentPayload(id string, i uint32, seed int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "mediad-segment:%s:seed=%d:", id, seed)
	for b := 0; b < 128; b++ {
		buf.WriteByte(byte((int(i)*31 + b*7 + int(seed)) & 0xff))
	}
	return buf.Bytes()
}

func recoverAndResume(work, journalPath, segID string, payload []byte,
	verifier func(string) error,
) (int, *txn.CommitResult, *txn.Journal, error) {
	j2, entries, err := txn.OpenJournal(journalPath)
	if err != nil {
		return 0, nil, nil, err
	}
	c2 := txn.NewCommitter(work, j2, txn.CrashNone)
	rerunIDs, err := c2.Recover(txn.Analyze(entries))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(rerunIDs) != 1 || rerunIDs[0] != segID {
		return 0, nil, nil, model.New(model.KindState, "pipeline", "recover",
			fmt.Sprintf("unexpected rerun set: %v", rerunIDs))
	}
	cr, err := c2.Run(segID, payload, verifier)
	if err != nil {
		return 0, nil, nil, err
	}
	return c2.RollbackCount, cr, j2, nil
}
