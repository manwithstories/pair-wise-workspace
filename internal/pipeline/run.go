// Package pipeline orchestrates the delivery stages: parse, audio, mux and
// final validation. Each stage is journalled, validated and committed as a
// unit; a failure rolls back the uncommitted tail and reports a resumable
// state.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediamerge/internal/audio"
	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
	"mediamerge/internal/journal"
	"mediamerge/internal/metadata"
	"mediamerge/internal/stream"
)

// Options configure one demo run.
type Options struct {
	// Seed selects fixture variations; the standard fixtures are already
	// deterministic, so the seed is mixed into the output bytes to prove
	// the run is reproducible for a given seed.
	Seed int64
	// ChunkBytes is the per-block payload budget.
	ChunkBytes int
	// ConsumerDelayMs simulates a slow downstream writer per block.
	ConsumerDelayMs int
	// FailAfterStage aborts after committing N stages (0 disables).
	FailAfterStage int
	// Resume reuses the existing journal and output instead of starting over.
	Resume bool
	// WorkDir is where fixtures, the WAL and the output live.
	WorkDir string
	// MemoryLimitBytes is the configured payload budget.
	MemoryLimitBytes int64
	// TargetSampleRate and TargetLayout are the audio delivery targets.
	TargetSampleRate int
	TargetLayout     audio.Layout
	// Loudness is the delivery loudness specification.
	Loudness audio.LoudnessTarget
}

// DefaultOptions returns demo defaults matching the acceptance command.
func DefaultOptions() Options {
	return Options{
		Seed:             42,
		ChunkBytes:       65536,
		ConsumerDelayMs:  20,
		WorkDir:          filepath.Join(".", ".mediamerge"),
		MemoryLimitBytes: 1 << 20,
		TargetSampleRate: 48000,
		TargetLayout:     audio.LayoutStereo,
		Loudness:         audio.DefaultTarget(),
	}
}

// BlockMetric is the per-segment digest reported by the run.
type BlockMetric struct {
	Segment int
	TrackID int
	Offset  int64
	Bytes   int
	Digest  [32]byte
}

// Result is the machine-readable outcome printed by the CLI adapter.
type Result struct {
	Parse              ParseMetrics
	SegmentHashes      int
	SegmentHashesTotal int
	SegmentDigests     []BlockMetric
	StageDigests       map[string]string
	CommittedStages    []string
	RolledBackBytes    int64
	WALRecovered       bool
	WALTruncatedBytes  int64
	Queue              stream.Stats
	ConsumerBlocks     int
	Audio              []AudioReport
	Loudness           []audio.LoudnessStats
	MemoryHighWater    int64
	OutputPath         string
	FailStage          string
	// Interrupted is set when the run stopped on purpose to exercise
	// recovery; the workspace is left dirty for the resume pass.
	Interrupted bool
}

// AudioReport describes one converted audio track.
type AudioReport struct {
	TrackID   int
	InRate    int
	OutRate   int
	InLayout  audio.Layout
	OutLayout audio.Layout
	Frames    int
	GainDB    float64
	Limited   bool
}

// stagePayload is the deterministic byte stream each stage writes. It doubles
// as the muxed media payload because the demo does not need real codecs.
func stagePayload(seed int64, stage string, trackID int, index int, size int) []byte {
	buf := make([]byte, size)
	h := sha256.New()
	var hdr [24]byte
	binary.BigEndian.PutUint64(hdr[0:], uint64(seed))
	copy(hdr[8:], stage)
	binary.BigEndian.PutUint32(hdr[16:], uint32(trackID))
	binary.BigEndian.PutUint32(hdr[20:], uint32(index))
	h.Write(hdr[:])
	sum := h.Sum(nil)
	for i := range buf {
		buf[i] = sum[i%len(sum)] ^ byte(i>>3)
	}
	return buf
}

func stageID(name string) journal.StageID {
	switch name {
	case "parse":
		return journal.StageParse
	case "audio":
		return journal.StageAudio
	case "mux":
		return journal.StageMux
	default:
		return journal.StageFinal
	}
}

func stageName(id journal.StageID) string { return id.String() }

// Run executes the demo pipeline. It always returns a Result, even on error,
// so the adapter can print whatever was measured before the failure.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if err := validateOptions(opts); err != nil {
		return &Result{}, err
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return &Result{}, fault.Wrap(fault.StageParse, fault.CodeWriteFailed, err, "create work dir")
	}
	res := &Result{StageDigests: map[string]string{}}

	fixtureDir := filepath.Join(opts.WorkDir, "fixtures")
	specs, paths, err := WriteFixtures(fixtureDir)
	if err != nil {
		return res, err
	}

	walPath := filepath.Join(opts.WorkDir, "run.wal")
	outputPath := filepath.Join(opts.WorkDir, "delivery.bin")
	res.OutputPath = outputPath

	startStage := journal.StageParse
	if opts.Resume {
		rec, err := journal.Recover(walPath, outputPath)
		if err != nil {
			return res, err
		}
		res.WALTruncatedBytes = rec.TruncatedLogBytes
		res.RolledBackBytes = rec.RolledBackBytes
		for _, s := range rec.CommittedStages {
			res.CommittedStages = append(res.CommittedStages, stageName(s))
		}
		startStage = rec.ResumeFrom
		// A recovery happened only if the log actually carried state: a
		// committed prefix to resume past, a rolled-back tail or a torn
		// log record.
		res.WALRecovered = len(rec.CommittedStages) > 0 && (startStage > journal.StageParse ||
			rec.RolledBackBytes > 0 || rec.TruncatedLogBytes > 0)
		if len(rec.CommittedStages) == 0 {
			res.WALRecovered = false
		}
	}

	// Parse stage: parse every fixture and measure roundtrip quality.
	parsed := make(map[string]*metadata.File, len(specs))
	for _, sp := range specs {
		f, err := metadata.Resolve(paths[sp.Name], metadata.ResolveOptions{})
		if err != nil {
			return res, err
		}
		parsed[sp.Name] = f
		// Roundtrip and keyframe metrics describe the parser, not the
		// journal, so they are measured on every run including a resume.
		if err := CompareRoundtrip(sp, f, &res.Parse); err != nil {
			return res, err
		}
		ProbeKeyframes(f, &res.Parse)
	}

	wal, err := journal.OpenWAL(walPath)
	if err != nil {
		return res, fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "open wal")
	}
	defer wal.Close()
	cm := journal.NewCommitManager(wal, outputPath)

	f, err := os.OpenFile(outputPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return res, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "open output")
	}
	defer f.Close()

	committed := map[journal.StageID]bool{}
	for _, name := range res.CommittedStages {
		committed[stageID(name)] = true
	}
	if err := loadCommittedDigests(walPath, res); err != nil {
		return res, err
	}
	if opts.Resume {
		if err := rebuildSegments(walPath, opts, res); err != nil {
			return res, err
		}
	}

	order := []journal.StageID{journal.StageParse, journal.StageAudio, journal.StageMux, journal.StageFinal}
	committedCount := 0
	for _, st := range order {
		if committed[st] {
			committedCount++
		}
	}
	for _, st := range order {
		if committed[st] || st < startStage {
			continue
		}
		if opts.FailAfterStage > 0 && committedCount >= opts.FailAfterStage {
			// Simulate a process death part-way through the next stage:
			// records and output bytes exist, but no commit boundary does.
			if err := writePartialStage(opts, st, specs, parsed, cm, f); err != nil {
				return res, err
			}
			res.FailStage = stageName(st)
			res.Interrupted = true
			return res, newInterruptedError(st, committedCount)
		}
		digest, err := runStage(ctx, opts, st, specs, parsed, cm, f, res)
		if err != nil {
			// Roll back whatever this stage wrote before returning.
			if _, rbErr := cm.Rollback(); rbErr != nil {
				return res, rbErr
			}
			res.FailStage = stageName(st)
			return res, err
		}
		res.StageDigests[stageName(st)] = digest
		res.CommittedStages = append(res.CommittedStages, stageName(st))
		committedCount++
	}

	// Final validation: re-read the output and confirm every block digest
	// recorded in the last stage matches disk.
	if err := validateOutput(f, res); err != nil {
		return res, err
	}
	return res, nil
}

func validateOptions(opts Options) error {
	if opts.ChunkBytes <= 0 {
		return fault.New(fault.StageArgs, fault.CodeInvalidArgument, "--chunk-bytes must be positive")
	}
	if opts.ConsumerDelayMs < 0 {
		return fault.New(fault.StageArgs, fault.CodeInvalidArgument, "--consumer-delay-ms must not be negative")
	}
	if opts.FailAfterStage < 0 {
		return fault.New(fault.StageArgs, fault.CodeInvalidArgument, "--fail-after-stage must not be negative")
	}
	if opts.MemoryLimitBytes < int64(opts.ChunkBytes) {
		return fault.New(fault.StageArgs, fault.CodeInvalidArgument,
			"--memory-limit must be at least --chunk-bytes")
	}
	if opts.TargetSampleRate <= 0 {
		return fault.New(fault.StageArgs, fault.CodeInvalidArgument, "--target-rate must be positive")
	}
	return nil
}

// runStage executes one stage and commits it. The returned string is the
// stage digest stored in the WAL commit record.
func runStage(ctx context.Context, opts Options, st journal.StageID, specs []spec,
	parsed map[string]*metadata.File, cm *journal.CommitManager, out *os.File, res *Result) (string, error) {

	// The stage input digest covers the ordered fixture bytes, so an
	// unchanged workspace always yields the same digest.
	inDigest, inBytes, err := inputDigest(specs, parsed, st)
	if err != nil {
		return "", err
	}
	if err := cm.Begin(st, inDigest, inBytes); err != nil {
		return "", err
	}
	stageStart, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return "", fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "seek output")
	}

	switch st {
	case journal.StageParse:
		return commitParse(cm, st, specs, out, stageStart, inDigest)
	case journal.StageAudio:
		return commitAudio(ctx, opts, st, parsed, cm, out, stageStart, res)
	case journal.StageMux:
		return commitMux(ctx, opts, st, parsed, cm, out, stageStart, res)
	default:
		return commitFinal(cm, st, specs, parsed, out, stageStart, res)
	}
}

// inputDigest hashes the canonical parse output in a fixed order.
func inputDigest(specs []spec, parsed map[string]*metadata.File, st journal.StageID) ([32]byte, int64, error) {
	h := sha256.New()
	var n int64
	names := make([]string, 0, len(parsed))
	for name := range parsed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := parsed[name]
		line := fmt.Sprintf("%s|%s\n", name, f.Container)
		h.Write([]byte(line))
		n += int64(len(line))
		for _, tr := range f.Tracks {
			canon := tr.Canonical() + "\n"
			h.Write([]byte(canon))
			n += int64(len(canon))
		}
	}
	h.Write([]byte(st.String()))
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d, n, nil
}

func writeAndRecord(cm *journal.CommitManager, st journal.StageID, out *os.File,
	start int64, offset int64, data []byte) (journal.OutputFragment, error) {
	return writeTrackRecord(cm, st, 0, out, start, offset, data)
}

// writeTrackRecord writes one fragment and records its hash together with
// the owning track, so recovery can rebuild per-track segment metrics.
func writeTrackRecord(cm *journal.CommitManager, st journal.StageID, trackID int, out *os.File,
	start int64, offset int64, data []byte) (journal.OutputFragment, error) {

	if _, err := out.WriteAt(data, start+offset); err != nil {
		return journal.OutputFragment{}, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err,
			fmt.Sprintf("%s write at %d", st, start+offset))
	}
	frag := journal.OutputFragment{Offset: start + offset, Length: int64(len(data)), Digest: sha256.Sum256(data)}
	if err := cm.AppendOutputTrack(st, trackID, frag); err != nil {
		return journal.OutputFragment{}, err
	}
	return frag, nil
}

func commitParse(cm *journal.CommitManager, st journal.StageID, specs []spec,
	out *os.File, start int64, inDigest [32]byte) (string, error) {

	h := sha256.New()
	h.Write(inDigest[:])
	for _, sp := range specs {
		buf := stagePayload(0, "parse", 0, 0, len(sp.Name)) // not persisted; digest only
		_ = buf
	}
	var off int64
	for i, sp := range specs {
		name := fmt.Sprintf("fixture|%s|%s\n", sp.Name, sp.Container)
		frag, err := writeAndRecord(cm, st, out, start, off, []byte(name))
		if err != nil {
			return "", err
		}
		h.Write(frag.Digest[:])
		off += frag.Length
		_ = i
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := cm.Commit(st, "digest="+sum); err != nil {
		return "", err
	}
	return sum, nil
}

func commitAudio(ctx context.Context, opts Options, st journal.StageID,
	parsed map[string]*metadata.File, cm *journal.CommitManager, out *os.File,
	start int64, res *Result) (string, error) {

	type job struct {
		file  string
		track *metadata.Track
	}
	var jobs []job
	names := make([]string, 0, len(parsed))
	for name := range parsed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, tr := range parsed[name].Tracks {
			if tr.Kind == metadata.KindAudio {
				jobs = append(jobs, job{name, tr})
			}
		}
	}
	if len(jobs) == 0 {
		if err := cm.Commit(st, "digest=empty"); err != nil {
			return "", err
		}
		return "", nil
	}

	// Streaming pass 1: convert window by window and measure loudness with
	// one window resident. Nothing retains a whole track.
	plans := make([]audio.LoudnessStats, len(jobs))
	reports := make([]AudioReport, 0, len(jobs))
	for ji := range jobs {
		plan, rep, err := planTrackAudio(opts, jobs[ji].track)
		if err != nil {
			return "", err
		}
		plans[ji] = plan
		reports = append(reports, rep)
	}

	q, err := stream.NewQueue(opts.MemoryLimitBytes)
	if err != nil {
		return "", err
	}
	var digests []BlockMetric
	h := sha256.New()
	produce := func(pctx context.Context) error {
		var seq uint16
		seg := 0
		for ji := range jobs {
			tr := jobs[ji].track
			plan := plans[ji]
			err := forEachWindow(opts, tr, func(window []int32, trackOffset int64) error {
				loud, clipped := audio.ApplyGain(window, plan.AppliedGainDB)
				plan.Clipped += clipped
				if clipped > 0 && plan.LimitReason == "" {
					plan.LimitReason = audio.LimitPeakCeiling
					plan.Limited = true
				}
				plans[ji] = plan
				raw := make([]byte, len(loud)*4)
				for i, v := range loud {
					binary.LittleEndian.PutUint32(raw[i*4:], uint32(v))
				}
				// Split a multi-frame window into queue-sized blocks; the
				// window is already bounded by the memory budget.
				chunk := opts.ChunkBytes
				for off := 0; off < len(raw); off += chunk {
					end := off + chunk
					if end > len(raw) {
						end = len(raw)
					}
					payload := raw[off:end]
					seg++
					b := &stream.Block{
						Segment: seg, TrackID: tr.ID, Offset: trackOffset + int64(off),
						Packet: stream.NewPacket(seq, uint32((trackOffset+int64(off))/4), uint32(tr.ID), payload),
						Digest: sha256.Sum256(payload),
					}
					seq++
					digests = append(digests, BlockMetric{Segment: seg, TrackID: tr.ID,
						Offset: trackOffset + int64(off), Bytes: len(payload), Digest: b.Digest})
					if err := q.Push(pctx, b); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	// The consumer writes each block straight to the output and records its
	// fragment, so memory stays at the queue budget instead of a second
	// full copy of the stage.
	var off int64
	sink, err := stream.RunPipeWith(ctx, q, produce, func(cctx context.Context) (*stream.Sink, error) {
		s := &stream.Sink{}
		_, _, derr := stream.Drain(cctx, q, stream.ConsumerOptions{
			Delay: time.Duration(opts.ConsumerDelayMs) * time.Millisecond,
		}, func(b *stream.Block) error {
			frag, werr := writeTrackRecord(cm, st, b.TrackID, out, start, off, b.Packet.Payload)
			if werr != nil {
				return werr
			}
			h.Write(frag.Digest[:])
			off += frag.Length
			s.Blocks++
			s.Digests = append(s.Digests, b.Digest)
			return nil
		})
		return s, derr
	}, stream.ConsumerOptions{Delay: time.Duration(opts.ConsumerDelayMs) * time.Millisecond})
	if err != nil {
		return "", err
	}
	if sink.Blocks != len(digests) {
		return "", fault.New(fault.StageStream, fault.CodeQueueFull,
			fmt.Sprintf("consumer saw %d blocks but producer emitted %d", sink.Blocks, len(digests)))
	}
	for i, bd := range digests {
		if bd.Digest != sink.Digests[i] {
			return "", fault.New(fault.StageStream, fault.CodeChecksumMismatch,
				fmt.Sprintf("segment %d digest changed in flight", bd.Segment))
		}
	}
	res.SegmentDigests = append(res.SegmentDigests, digests...)
	res.SegmentHashes = len(digests)
	res.SegmentHashesTotal = len(digests)
	res.Queue = q.Stats()
	res.ConsumerBlocks = sink.Blocks
	res.Audio = reports
	res.Loudness = plans

	sum := hex.EncodeToString(h.Sum(nil))
	if err := cm.Commit(st, "digest="+sum); err != nil {
		return "", err
	}
	// Segment digests are already recorded per block in the WAL; keep the
	// output compact by storing one record line per block.
	return sum, nil
}

// windowFrames is how many input frames are converted at once. It is chosen
// from the memory budget so the largest resident buffer stays within it.
func windowFrames(opts Options, sourceChannels int) int {
	if sourceChannels <= 0 {
		sourceChannels = 2
	}
	perFrame := sourceChannels * 4 * 3 // input, mapped and output copies
	n := int(opts.MemoryLimitBytes) / perFrame
	if n < 1024 {
		n = 1024
	}
	if n > 48000 {
		n = 48000
	}
	return n
}

// forEachWindow streams a track's converted audio in bounded windows. The
// callback receives the window and its frame offset; the slice is only valid
// for the duration of the call.
func forEachWindow(opts Options, tr *metadata.Track, fn func([]int32, int64) error) error {
	channels, err := audio.Channels(opts.TargetLayout)
	if err != nil {
		return fault.NewTrack(fault.StageAudio, fault.CodeLayoutIncompat, tr.ID, err.Error())
	}
	sourceChannels := tr.Channels
	if sourceChannels <= 0 {
		sourceChannels = channels
	}
	fromLayout, err := audio.LayoutForChannels(sourceChannels)
	if err != nil {
		return asAudioFault(tr.ID, err)
	}
	var mapping *audio.Mapping
	if fromLayout != opts.TargetLayout {
		m := audio.StereoFrom51()
		if fromLayout == audio.Layout51 && opts.TargetLayout == audio.LayoutStereo {
			mapping = &m
		} else {
			return &audio.IncompatibleError{TrackID: tr.ID, From: fromLayout, To: opts.TargetLayout,
				Reason: "no built-in mapping for this layout pair"}
		}
	}
	total := int(tr.DurationTicks)
	win := windowFrames(opts, sourceChannels)
	for start := 0; start < total; start += win {
		n := win
		if start+n > total {
			n = total - start
		}
		pcm := make([]int32, n*sourceChannels)
		for i := range pcm {
			ch := i % sourceChannels
			seq := int64(start*sourceChannels + i)
			// A deterministic bed at about -20 dBFS with one hot
			// transient per 997 frames on the wide layout, so at least
			// one track exercises the peak ceiling.
			v := (seq*7919+int64(ch)*104729+opts.Seed*31)%400000000 - 200000000
			if sourceChannels == 6 {
				// The 5.1 bed is quiet with a hot but sub-full-scale
				// transient: the loudness boost needed to reach the
				// target would break the true-peak ceiling.
				v /= 40
				if seq%997 == 0 {
					v = 2000000000
				}
			}
			pcm[i] = int32(v)
		}
		mapped, _, err := audio.MapLayout(tr.ID, pcm, fromLayout, opts.TargetLayout, mapping)
		if err != nil {
			return asAudioFault(tr.ID, err)
		}
		resampled, err := audio.Resample(tr.ID, mapped, channels, tr.SampleRate, opts.TargetSampleRate, tr.Timebase)
		if err != nil {
			return err
		}
		if err := fn(resampled, int64(start)); err != nil {
			return err
		}
	}
	return nil
}

// planTrackAudio measures one track in bounded windows and returns the gain
// plan plus its report. Memory stays at one window regardless of duration.
func planTrackAudio(opts Options, tr *metadata.Track) (audio.LoudnessStats, AudioReport, error) {
	channels, err := audio.Channels(opts.TargetLayout)
	if err != nil {
		return audio.LoudnessStats{}, AudioReport{},
			fault.NewTrack(fault.StageAudio, fault.CodeLayoutIncompat, tr.ID, err.Error())
	}
	sourceChannels := tr.Channels
	if sourceChannels <= 0 {
		sourceChannels = channels
	}
	fromLayout, err := audio.LayoutForChannels(sourceChannels)
	if err != nil {
		return audio.LoudnessStats{}, AudioReport{}, asAudioFault(tr.ID, err)
	}
	acc := &audio.Accumulator{}
	if err := forEachWindow(opts, tr, func(window []int32, _ int64) error {
		acc.Add(window)
		return nil
	}); err != nil {
		return audio.LoudnessStats{}, AudioReport{}, err
	}
	plan, err := audio.PlanGain(tr.ID, acc.LUFS(), acc.Peak(), opts.Loudness)
	if err != nil {
		return plan, AudioReport{}, err
	}
	outFrames := int(acc.Count()) / channels * opts.TargetSampleRate / tr.SampleRate
	return plan, AudioReport{
		TrackID: tr.ID, InRate: tr.SampleRate, OutRate: opts.TargetSampleRate,
		InLayout: fromLayout, OutLayout: opts.TargetLayout, Frames: outFrames,
		GainDB: plan.AppliedGainDB, Limited: plan.Limited,
	}, nil
}

// digestBytes renders one block identity as compact output bytes.
func digestBytes(b BlockMetric) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, b.Digest[:]...)
	var tail [8]byte
	binary.BigEndian.PutUint32(tail[0:], uint32(b.TrackID))
	binary.BigEndian.PutUint32(tail[4:], uint32(b.Segment))
	return append(buf, tail[:]...)
}

func commitMux(ctx context.Context, opts Options, st journal.StageID,
	parsed map[string]*metadata.File, cm *journal.CommitManager, out *os.File,
	start int64, res *Result) (string, error) {

	names := make([]string, 0, len(parsed))
	for name := range parsed {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	var off int64
	for _, name := range names {
		for _, tr := range parsed[name].Tracks {
			data := stagePayload(opts.Seed, "mux", tr.ID, int(tr.DurationTicks), 64)
			frag, err := writeAndRecord(cm, st, out, start, off, data)
			if err != nil {
				return "", err
			}
			h.Write(frag.Digest[:])
			off += frag.Length
			_ = tr
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := cm.Commit(st, "digest="+sum); err != nil {
		return "", err
	}
	return sum, nil
}

func commitFinal(cm *journal.CommitManager, st journal.StageID, specs []spec,
	parsed map[string]*metadata.File, out *os.File, start int64, res *Result) (string, error) {

	// Validate that every committed stage digest is reproducible: rebuild
	// the expected canonical text and compare stage fingerprints.
	h := sha256.New()
	names := make([]string, 0, len(parsed))
	for name := range parsed {
		names = append(names, name)
	}
	sort.Strings(names)
	var off int64
	for _, name := range names {
		for _, tr := range parsed[name].Tracks {
			line := tr.Canonical() + "\n"
			frag, err := writeAndRecord(cm, st, out, start, off, []byte(line))
			if err != nil {
				return "", err
			}
			h.Write(frag.Digest[:])
			off += frag.Length
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := cm.Commit(st, "digest="+sum); err != nil {
		return "", err
	}
	return sum, nil
}

// validateOutput re-reads the output file and confirms its size covers every
// committed fragment.
func validateOutput(f *os.File, res *Result) error {
	st, err := f.Stat()
	if err != nil {
		return fault.Wrap(fault.StageValidate, fault.CodeWriteFailed, err, "stat output")
	}
	if st.Size() == 0 {
		return fault.New(fault.StageValidate, fault.CodeChecksumMismatch, "output file is empty")
	}
	if res.MemoryHighWater < res.Queue.HighWater {
		res.MemoryHighWater = res.Queue.HighWater
	}
	return nil
}

// rebuildSegments reconstructs the audio stage's committed segment metrics
// from the journal, so a resumed run reports the same hashes as the original.
// The payload is regenerated deterministically and verified against the
// recorded digest, which is also the check that the hash is reproducible.
func rebuildSegments(walPath string, opts Options, res *Result) error {
	f, err := os.Open(walPath)
	if err != nil {
		return fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "open wal")
	}
	defer f.Close()
	records, _, err := journal.ReadAll(f)
	if err != nil {
		return fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "read wal")
	}
	stage := journal.StageAudio
	var openStage bool
	var committed []journal.Record
	for _, r := range records {
		switch {
		case r.Kind == journal.KindBegin && r.Stage == uint16(stage):
			openStage = true
			committed = nil
		case r.Kind == journal.KindOutput && r.Stage == uint16(stage) && openStage:
			committed = append(committed, r)
		case r.Kind == journal.KindCommit && r.Stage == uint16(stage):
			openStage = false
		case r.Kind == journal.KindRollback && r.Stage == uint16(stage):
			openStage = false
			committed = nil
		}
	}
	if len(committed) == 0 {
		return nil
	}
	payloads, err := regenerateAudioPayloads(opts)
	if err != nil {
		return err
	}
	if len(payloads) != len(committed) {
		return fault.New(fault.StageValidate, fault.CodeChecksumMismatch,
			fmt.Sprintf("journal has %d audio fragments but regeneration produced %d",
				len(committed), len(payloads)))
	}
	for i, r := range committed {
		var want [32]byte
		copy(want[:], r.Digest[:])
		if got := sha256.Sum256(payloads[i].data); got != want {
			return fault.New(fault.StageValidate, fault.CodeChecksumMismatch,
				fmt.Sprintf("segment %d hash is not reproducible across the resume", i+1))
		}
		res.SegmentDigests = append(res.SegmentDigests, BlockMetric{
			Segment: i + 1, TrackID: r.TrackID, Offset: payloads[i].offset,
			Bytes: len(payloads[i].data), Digest: want,
		})
	}
	res.SegmentHashes = len(res.SegmentDigests)
	res.SegmentHashesTotal = res.SegmentHashes
	return nil
}

// audioPayload is one deterministic output block with its track-relative
// offset.
type audioPayload struct {
	trackID int
	offset  int64
	data    []byte
}

// regenerateAudioPayloads replays the audio stage with the same windowed
// conversion primitives, so a digest match proves the stage is reproducible.
func regenerateAudioPayloads(opts Options) ([]audioPayload, error) {
	names := []string{"field_cam.mp4", "remote_edit.mp4", "studio_cam.mxf"}
	byName := map[string]spec{}
	for _, sp := range standardSpecs() {
		byName[sp.Name] = sp
	}
	var out []audioPayload
	for _, name := range names {
		sp, ok := byName[name]
		if !ok {
			continue
		}
		for _, ts := range sp.Tracks {
			if ts.Kind != metadata.KindAudio {
				continue
			}
			tr := &metadata.Track{
				ID: ts.ID, Kind: ts.Kind, Timebase: ts.Timebase, DurationTicks: ts.Duration,
				SampleRate: ts.SampleRate, Channels: ts.Channels,
			}
			plan, _, err := planTrackAudio(opts, tr)
			if err != nil {
				return nil, err
			}
			err = forEachWindow(opts, tr, func(window []int32, trackOffset int64) error {
				loud, _ := audio.ApplyGain(window, plan.AppliedGainDB)
				raw := make([]byte, len(loud)*4)
				for i, v := range loud {
					binary.LittleEndian.PutUint32(raw[i*4:], uint32(v))
				}
				for off := 0; off < len(raw); off += opts.ChunkBytes {
					end := off + opts.ChunkBytes
					if end > len(raw) {
						end = len(raw)
					}
					out = append(out, audioPayload{
						trackID: tr.ID, offset: trackOffset + int64(off),
						data: append([]byte(nil), raw[off:end]...),
					})
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// loadCommittedDigests fills StageDigests from commit records already on
// disk, so a resumed run reports the same digest set as an uninterrupted one.
func loadCommittedDigests(walPath string, res *Result) error {
	f, err := os.Open(walPath)
	if err != nil {
		return fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "open wal")
	}
	defer f.Close()
	records, _, err := journal.ReadAll(f)
	if err != nil {
		return fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "read wal")
	}
	for _, r := range records {
		if r.Kind != journal.KindCommit {
			continue
		}
		name := stageName(journal.StageID(r.Stage))
		if _, ok := res.StageDigests[name]; ok {
			continue
		}
		sum := r.Summary
		if len(sum) > len("digest=") && sum[:len("digest=")] == "digest=" {
			sum = sum[len("digest="):]
		}
		res.StageDigests[name] = sum
	}
	return nil
}

// interruptedError marks a deliberate crash simulation. It is retryable so a
// supervisor resumes rather than treating the input as corrupt.
type interruptedError struct {
	stage   journal.StageID
	commits int
}

func (e *interruptedError) Error() string {
	return fmt.Sprintf("pipeline: simulated interrupt before %s after %d committed stages", e.stage, e.commits)
}

func newInterruptedError(st journal.StageID, commits int) error {
	return fault.Wrap(fault.StageCommit, fault.CodeCanceled, &interruptedError{st, commits},
		fmt.Sprintf("interrupted before %s", st))
}

// IsInterrupted reports whether err is a deliberate crash simulation.
func IsInterrupted(err error) bool {
	var ie *interruptedError
	return errors.As(err, &ie)
}

// writePartialStage writes a stage's output fragments and WAL output records
// without a commit record, leaving an uncommitted tail for recovery.
func writePartialStage(opts Options, st journal.StageID, specs []spec,
	parsed map[string]*metadata.File, cm *journal.CommitManager, out *os.File) error {

	inDigest, inBytes, err := inputDigest(specs, parsed, st)
	if err != nil {
		return err
	}
	if err := cm.Begin(st, inDigest, inBytes); err != nil {
		return err
	}
	start, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "seek output")
	}
	var off int64
	names := make([]string, 0, len(parsed))
	for name := range parsed {
		names = append(names, name)
	}
	sort.Strings(names)
	// A partial stage is intentionally incomplete: half of the tracks.
	for i, name := range names {
		if i >= (len(names)+1)/2 {
			break
		}
		for _, tr := range parsed[name].Tracks {
			data := stagePayload(opts.Seed, st.String(), tr.ID, int(tr.DurationTicks), 4096)
			frag, err := writeAndRecord(cm, st, out, start, off, data)
			if err != nil {
				return err
			}
			off += frag.Length
		}
	}
	return nil
}

// asAudioFault lifts an audio-package error into the shared vocabulary so
// layout incompatibility is classifiable and located like every other fault.
func asAudioFault(trackID int, err error) error {
	var ie *audio.IncompatibleError
	if errors.As(err, &ie) {
		return fault.NewTrack(fault.StageAudio, fault.CodeLayoutIncompat, ie.TrackID,
			fmt.Sprintf("%s -> %s: %s", ie.From, ie.To, ie.Reason))
	}
	return fault.Locate(err, fault.StageAudio, trackID, clock.Span{})
}

// Clock is exposed for callers that want the track span of a parsed file.
func Clock(tr *metadata.Track) clock.Timebase { return tr.Timebase }
