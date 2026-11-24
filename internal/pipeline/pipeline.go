// Package pipeline wires every relay subsystem together: local fixture
// generation, demux, RTP recovery, clock normalization, track mapping,
// sample-preserving remux and the crash-safe transactional commit.
package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"time"

	"relay/internal/clock"
	"relay/internal/demux"
	"relay/internal/fixture"
	"relay/internal/mapping"
	"relay/internal/media"
	"relay/internal/metrics"
	"relay/internal/recovery"
	"relay/internal/safepath"
	"relay/internal/txn"
)

// Exit codes of the terminal entry point.
const (
	ExitOK         = 0
	ExitUsage      = 2
	ExitMediaError = 3
	ExitInterrupt  = 75
)

// Config selects the demo scenario.
type Config struct {
	WorkDir    string
	Damage     string // "" | "checksum"
	CrashAfter string // "" | "staged"
}

// Outcome is everything the terminal layer prints or acts on.
type Outcome struct {
	Report        *metrics.Report
	ExitCode      int
	InputFiles    map[string][]byte
	RTPResult     *recovery.Result
	Tracks        map[uint32]*mapping.Track
	InputDigest   [32]byte
	OutputDigest  [32]byte
	ManifestSum   [32]byte
	CurrentBefore [32]byte
	CurrentAfter  [32]byte
	HadCurrent    bool
}

// Run executes one full pipeline scenario inside conf.WorkDir.
func Run(conf Config) (*Outcome, error) {
	root, err := safepath.NewRoot(conf.WorkDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root.String(), 0o755); err != nil {
		return nil, err
	}
	collector := &metrics.Collector{}

	// 1. Generate the three local capture sources (studio video, remote
	//    narration audio, exterior RTP video) with mixed frame rates and drift.
	videoSpec := fixture.Spec{
		SourceID: 1, Kind: media.KindVideo, TimescaleHz: 30, ClockRateHz: 90_000,
		Width: 1280, Height: 720, Samples: 72, DriftPPM: 12,
		StartRefNS: 1_000_000, PayloadSeed: 101,
	}
	audioSpec := fixture.Spec{
		SourceID: 2, Kind: media.KindAudio, TimescaleHz: 50, ClockRateHz: 48_000,
		Samples: 60, DriftPPM: -8, StartRefNS: 2_000_000, PayloadSeed: 202,
	}
	rtpSpec := fixture.Spec{
		SourceID: 3, Kind: media.KindVideo, TimescaleHz: 24, ClockRateHz: 90_000,
		Width: 960, Height: 540, Samples: 60, DriftPPM: 5,
		StartRefNS: 3_000_000, PayloadSeed: 303,
	}
	videoFrames := fixture.Frames(videoSpec)
	audioFrames := fixture.Frames(audioSpec)

	// 2. Serialize inputs; the RTP log holds one recoverable single loss,
	//    one duplicate and an adjacent-packet reorder.
	fseqBytes := fixture.FSEQFile(videoSpec, videoFrames)
	mp4Bytes := fixture.MP4File(audioSpec, audioFrames)
	rtpBytes := fixture.RTPLog(rtpSpec, fixture.RTPImpairments{
		SingleLoss:  []uint16{9}, // group 1: only seq 9 missing -> XOR repair
		DuplicateAt: []uint16{20},
		Reorder:     true,
	})
	if conf.Damage == "checksum" {
		fseqBytes = damageChecksum(fseqBytes)
	}
	inputFiles := map[string][]byte{
		"video.fseq": fseqBytes, "audio.mp4": mp4Bytes, "remote.rtpl": rtpBytes,
	}
	inputDigest := digestInputs(inputFiles)
	if err := persistInputs(root, inputFiles); err != nil {
		return nil, err
	}

	// 3. Open the transaction before validation so a failure can roll back.
	t, err := txn.Begin(root.String(), inputDigest, conf.CrashAfter)
	if err != nil {
		return nil, err
	}
	outcome := &Outcome{
		InputFiles: inputFiles, InputDigest: inputDigest,
		Report: &metrics.Report{Mode: "demo"},
	}
	outcome.CurrentBefore, outcome.HadCurrent = txn.CurrentCommitID(root.String())

	// 4. Demux all sources; validation failures trigger a real rollback.
	demuxStart := time.Now()
	videoIn, err := demux.ReadFSEQ(bytes.NewReader(fseqBytes))
	if err != nil {
		return rollbackOutcome(t, root, outcome, collector, err)
	}
	collector.RecordBatch(time.Since(demuxStart))
	mp4Start := time.Now()
	mp4In, _, err := demux.ReadFragmentedMP4(bytes.NewReader(mp4Bytes))
	if err != nil {
		return rollbackOutcome(t, root, outcome, collector, err)
	}
	collector.RecordBatch(time.Since(mp4Start))
	rtpReadStart := time.Now()
	rtpRecords, rtpHdr, err := demux.ReadRTPLog(bytes.NewReader(rtpBytes))
	if err != nil {
		return rollbackOutcome(t, root, outcome, collector, err)
	}
	collector.RecordBatch(time.Since(rtpReadStart))

	// 5. RTP recovery: order, dedupe, XOR single loss, mark residual gaps.
	recStart := time.Now()
	rtpRes, err := recovery.Recover(rtpRecords, rtpHdr)
	if err != nil {
		return rollbackOutcome(t, root, outcome, collector, err)
	}
	collector.RecordBatch(time.Since(recStart))

	// 6. Normalize every source onto the unified nanosecond time base.
	clockStart := time.Now()
	allFrames := make([]*media.RawFrame, 0, len(videoIn)+len(mp4In)+len(rtpRes.Frames))
	allFrames = append(allFrames, videoIn...)
	allFrames = append(allFrames, mp4In...)
	allFrames = append(allFrames, rtpRes.Frames...)
	ordered := clock.AssignPresentation(allFrames)
	collector.RecordBatch(time.Since(clockStart))

	// 7. Map sources to tracks; duplicates, gaps and conflicts are rejected.
	rules := []mapping.Rule{
		{SourceID: videoSpec.SourceID, Kind: media.KindVideo, TrackID: 1},
		{SourceID: audioSpec.SourceID, Kind: media.KindAudio, TrackID: 2},
		{SourceID: rtpSpec.SourceID, Kind: media.KindVideo, TrackID: 3},
	}
	mapper, err := mapping.NewMapper(rules)
	if err != nil {
		return rollbackOutcome(t, root, outcome, collector, err)
	}
	tracks, err := mapper.Map(ordered)
	if err != nil {
		return rollbackOutcome(t, root, outcome, collector, err)
	}
	outcome.Tracks = tracks
	outcome.RTPResult = rtpRes
	outcome.Report.Samples = len(ordered)

	// 8. Remux preserving sample bytes, then stage and atomically commit.
	if err := remuxAndCommit(conf, root, t, mapper, tracks, allFrames,
		rtpRes, collector, outcome); err != nil {
		return nil, err
	}
	outcome.CurrentAfter, _ = txn.CurrentCommitID(root.String())
	outcome.Report.CurrentChanged = outcome.HadCurrent &&
		outcome.CurrentAfter != outcome.CurrentBefore
	return outcome, nil
}

// digestInputs hashes the fixed-order set of input files for the commit id.
func digestInputs(files map[string][]byte) [32]byte {
	names := []string{"audio.mp4", "remote.rtpl", "video.fseq"}
	h := sha256.New()
	for _, name := range names {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(files[name])))
		h.Write(l[:])
		h.Write(files[name])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// persistInputs writes the generated inputs under <work>/inputs for audit.
func persistInputs(root *safepath.Root, files map[string][]byte) error {
	dir, err := root.Join("inputs")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range files {
		p, err := root.Join("inputs", name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
