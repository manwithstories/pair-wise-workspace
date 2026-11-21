// Package pipeline wires the archive stages together: EBML parse, block
// de-lacing, track mapping, bounded queue hand-off, MP4 remux, independent
// re-read verification and keyframe indexing.
//
// Frame payloads are spooled to a temporary file as they are demuxed. Only
// the blocks in the bounded queue are resident in memory, so peak memory is
// capacity times MaxBlockBytes regardless of file length.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"archivecheck.local/archivecheck/internal/clock"
	"archivecheck.local/archivecheck/internal/demux"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fsguard"
	"archivecheck.local/archivecheck/internal/index"
	"archivecheck.local/archivecheck/internal/remux"
	"archivecheck.local/archivecheck/internal/stream"
	"archivecheck.local/archivecheck/internal/track"
	"archivecheck.local/archivecheck/internal/verify"
)

// Options configure a run.
type Options struct {
	// InputPath is the Matroska/WebM source, resolved under Root.
	InputPath string
	// OutputPath is the MP4 target, resolved under Root.
	OutputPath string
	// Recovery enables skipping damage whose boundaries can be proven.
	Recovery bool
	// QueueCapacity bounds the number of in-flight blocks.
	QueueCapacity int
	// SlowConsumerMS delays the consumer per block, exercising backpressure.
	SlowConsumerMS int
	// MaxBlockBytes caps one block payload and therefore resident memory.
	MaxBlockBytes int64
	// MovieTimescale is the MP4 movie timescale.
	MovieTimescale int64
	// Root confines both paths.
	Root *fsguard.Root
	// RefuseUnknownCodecs turns a track with an unsupported codec into a hard
	// error instead of a counted omission. The default (false) reports the
	// count and remuxes the supported tracks only.
	RefuseUnknownCodecs bool
	// RTPCheck validates each spooled sample's RTP packet structure using
	// pion/rtp. It is optional and off by default.
	RTPCheck bool
}

// Report is the outcome of a run.
type Report struct {
	Info    demux.Info
	Tracks  []track.TrackSummary
	Unknown []string

	Stats               map[uint64]*demux.TrackStats
	UnknownTrackBlocks  int64
	UnknownTrackNumbers []uint64

	Skipped      []fault.Skip
	SkippedBytes int64

	InputPath    string
	OutputPath   string
	OutputSize   int64
	OutputSHA256 string

	Manifest     []remux.ManifestEntry
	Verification *verify.Result
	Rates        verify.Rates
	Index        index.Index

	Queue         stream.Stats
	MappingDigest string
	Timebase      clock.Timebase
	// RTPPackets is the number of valid RTP packets when RTPCheck was on.
	RTPPackets int
}

// ScanResult is what a parse-only pass reports.
type ScanResult struct {
	Info                demux.Info
	Tracks              []track.TrackSummary
	Unknown             []string
	Stats               map[uint64]*demux.TrackStats
	UnknownTrackBlocks  int64
	UnknownTrackNumbers []uint64
	Skipped             []fault.Skip
	SkippedBytes        int64
}

// Scan parses only, without writing output. It is used by verify fixtures and
// by the report path when no output is requested.
func Scan(ctx context.Context, root *fsguard.Root, path string, recovery bool) (*ScanResult, error) {
	if root == nil {
		return nil, fault.New(fault.StageOpen, errors.New("input root is required"))
	}
	f, _, err := root.Open(path)
	if err != nil {
		return nil, fault.New(fault.StageOpen, err)
	}
	defer f.Close()
	var res demux.Result
	opts := demux.DefaultOptions()
	opts.Recover = recovery
	opts.CollectSamples = true
	opts.OnSample = func(demux.Sample) error {
		select {
		case <-ctx.Done():
			return fault.New(fault.StageQueue, context.Cause(ctx))
		default:
			return nil
		}
	}
	if err := demux.ScanReader(f, opts, &res); err != nil {
		return nil, classifyRecovery(err, res.Recovery, recovery)
	}
	out := &ScanResult{
		Info:                res.Info,
		Stats:               res.Stats,
		UnknownTrackBlocks:  res.UnknownTrackBlocks,
		UnknownTrackNumbers: res.UnknownTrackNumbers,
		Skipped:             res.Recovery,
	}
	tracks, unknown, err := track.Map(res.Tracks)
	if err != nil {
		return nil, err
	}
	report := track.NewReport(tracks, unknown)
	out.Tracks = report.Tracks
	out.Unknown = report.UnknownCodecIDs
	for _, s := range out.Skipped {
		out.SkippedBytes += s.Length
	}
	return out, nil
}

// Run performs a full archive pass: parse, map, spool, remux, verify, index.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.QueueCapacity <= 0 {
		return nil, fault.New(fault.StageQueue, errors.New("queue capacity must be positive"))
	}
	if opts.MovieTimescale <= 0 {
		opts.MovieTimescale = remux.DefaultMovieTimescale
	}
	if opts.MaxBlockBytes <= 0 {
		opts.MaxBlockBytes = 16 << 20
	}
	if opts.Root == nil {
		return nil, fault.New(fault.StageOpen, errors.New("input root is required"))
	}
	input, _, err := opts.Root.Open(opts.InputPath)
	if err != nil {
		return nil, fault.New(fault.StageOpen, err)
	}
	defer input.Close()
	outPath, err := opts.Root.GuardOutput(opts.InputPath, opts.OutputPath)
	if err != nil {
		return nil, fault.New(fault.StageOpen, err)
	}

	spool, err := os.CreateTemp("", "archivecheck-spool-*")
	if err != nil {
		return nil, fault.New(fault.StageIO, err)
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()

	q, err := stream.New[remux.Sample](opts.QueueCapacity)
	if err != nil {
		return nil, err
	}

	// The consumer only records metadata and applies the optional delay; the
	// producer already spooled the payload, so nothing is written twice.
	// Metadata is ~64 bytes per sample; payloads stay on disk.
	var (
		mu       sync.Mutex
		consumer error
		wg       sync.WaitGroup
	)
	perTrack := map[uint64][]remux.Sample{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			s, err := q.RecvCtx(ctx)
			if err != nil {
				if errors.Is(err, stream.ErrClosed) {
					return
				}
				mu.Lock()
				consumer = err
				mu.Unlock()
				return
			}
			if opts.SlowConsumerMS > 0 {
				select {
				case <-time.After(time.Duration(opts.SlowConsumerMS) * time.Millisecond):
				case <-ctx.Done():
					mu.Lock()
					consumer = fault.New(fault.StageQueue, context.Cause(ctx))
					mu.Unlock()
					return
				}
			}
			s.Data = nil
			mu.Lock()
			perTrack[s.TrackNumber] = append(perTrack[s.TrackNumber], s)
			mu.Unlock()
		}
	}()

	var (
		spoolPos int64
		spoolErr error
	)
	scanOpts := demux.DefaultOptions()
	scanOpts.MaxBlockBytes = opts.MaxBlockBytes
	scanOpts.Recover = opts.Recovery
	scanOpts.CollectSamples = true
	scanOpts.OnSample = func(s demux.Sample) error {
		if spoolErr != nil {
			return spoolErr
		}
		if _, err := spool.WriteAt(s.Data, spoolPos); err != nil {
			spoolErr = fault.New(fault.StageIO, err)
			return spoolErr
		}
		off := spoolPos
		spoolPos += int64(len(s.Data))
		if err := q.Send(ctx, remux.Sample{
			TrackNumber:  s.TrackNumber,
			Reader:       spool,
			Offset:       off,
			Size:         int64(len(s.Data)),
			SourceOffset: s.SourceOffset,
			DTS:          s.Timestamp,
			PTS:          s.Timestamp,
			DurationTick: s.DurationTicks,
			Keyframe:     s.Keyframe,
			Tick:         s.Timestamp,
		}); err != nil {
			// Attach the source byte that was in flight so a cancelled or
			// blocked hand-off is actionable, not just "stage=queue".
			var fe *fault.Error
			if errors.As(err, &fe) {
				if fe.Offset < 0 {
					fe.Offset = s.SourceOffset
				}
				if fe.Track == 0 {
					fe.Track = s.TrackNumber
				}
				return fe
			}
			return err
		}
		return nil
	}
	var scanRes demux.Result
	scanErr := demux.ScanReader(input, scanOpts, &scanRes)
	if scanErr == nil {
		scanErr = spoolErr
	}
	q.CloseWith(scanErr)
	wg.Wait()
	if consumer != nil && scanErr == nil {
		scanErr = consumer
	}
	if scanErr != nil {
		return nil, classifyRecovery(scanErr, scanRes.Recovery, opts.Recovery)
	}

	tb, err := clock.NewTimebase(scanRes.Info.TimestampScale)
	if err != nil {
		return nil, fault.New(fault.StageMap, err)
	}
	tracks, unknown, err := track.Map(scanRes.Tracks)
	if err != nil {
		return nil, err
	}
	trackReport := track.NewReport(tracks, unknown)

	// The contract is explicit: a track using a codec with no documented MP4
	// mapping is counted and refused, never silently dropped or transcoded.
	if opts.RefuseUnknownCodecs && len(unknown) > 0 {
		return nil, fault.New(fault.StageMap,
			fmt.Errorf("%w: %s", fault.ErrUnsupportedCodec, strings.Join(trackReport.UnknownCodecIDs, ","))).OnTrack(unknown[0].Number)
	}

	// Map each track through the shared timebase.
	plan := &remux.Plan{MovieTimescale: opts.MovieTimescale, Brand: "isom"}
	var digestParts []string
	for i := range tracks {
		tr := tracks[i]
		list := perTrack[tr.Number]
		samples := make([]clock.Sample, len(list))
		for j, s := range list {
			samples[j] = clock.Sample{Index: j, DTS: s.DTS, PTS: s.PTS, Duration: s.DurationTick}
		}
		m, err := clock.MapSamples(tb, clock.DefaultTimescale, samples, int64(tr.DefaultDuration))
		if err != nil {
			return nil, fault.New(fault.StageMap, err).OnTrack(tr.Number)
		}
		digestParts = append(digestParts, fmt.Sprintf("%d:%s", tr.Number, m.Digest()))
		tp := &remux.TrackPlan{
			Track:          tr,
			Timescale:      clock.DefaultTimescale,
			TimescaleMovie: opts.MovieTimescale,
			Shift:          m.Origin,
		}
		if tr.Type == track.TypeVideo {
			tp.Width, tp.Height = int(tr.PixelWidth), int(tr.PixelHeight)
			tp.Handler = "vide"
		} else if tr.Type == track.TypeAudio {
			tp.SampleRate, tp.Channels = int(tr.SamplingFrequency), int(tr.Channels)
			tp.Handler = "soun"
		} else {
			tp.Handler = "sbtl"
		}
		tp.Language = tr.Language
		// MapSamples sorts by DTS, so pair every mapped timing with the
		// source sample that produced it rather than with the same slice
		// position.
		ordered := make([]remux.Sample, len(list))
		for outIdx, srcIdx := range m.SourceOrder {
			ms := m.Samples[outIdx]
			smp := list[srcIdx]
			smp.DTS = ms.DTS
			smp.PTS = ms.PTS
			smp.Duration = ms.Duration
			ordered[outIdx] = smp
		}
		tp.Samples = ordered
		plan.Tracks = append(plan.Tracks, tp)
	}
	trackReport.Tracks = annotate(trackReport.Tracks, scanRes.Stats)

	atomic, err := fsguard.CreateOutput(outPath)
	if err != nil {
		return nil, fault.New(fault.StageIO, err)
	}
	sum, err := remux.Write(atomic, plan)
	if err != nil {
		_ = atomic.Abort()
		return nil, err
	}
	if err := atomic.Sync(); err != nil {
		_ = atomic.Abort()
		return nil, fault.New(fault.StageIO, err)
	}

	// Verify from the completed temporary file before it is renamed into
	// place, so a failed verification leaves no output behind.
	outFile, err := os.Open(atomic.Name())
	if err != nil {
		_ = atomic.Abort()
		return nil, fault.New(fault.StageIO, err)
	}
	defer outFile.Close()
	inputSize, _ := input.Seek(0, io.SeekEnd)
	_, _ = input.Seek(0, io.SeekStart)
	vr, err := verify.Check(outFile, sum.Size, sum.Manifest, verify.Options{
		Input: input, InputSize: inputSize,
	})
	if err != nil {
		_ = atomic.Abort()
		return nil, err
	}
	if err := atomic.Commit(); err != nil {
		return nil, fault.New(fault.StageIO, err)
	}
	sha, err := hashFile(outPath)
	if err != nil {
		return nil, err
	}

	// Build the keyframe index from the decoded blocks and corroborate it
	// with the source Cues where those are trustworthy.
	ix := buildIndex(&scanRes)

	var rtpPackets int
	if opts.RTPCheck {
		n, err := validateSpooledRTP(spool, perTrack)
		if err != nil {
			return nil, err
		}
		rtpPackets = n
	}

	rep := &Report{
		Info:                scanRes.Info,
		InputPath:           opts.InputPath,
		Tracks:              trackReport.Tracks,
		Unknown:             trackReport.UnknownCodecIDs,
		Stats:               scanRes.Stats,
		UnknownTrackBlocks:  scanRes.UnknownTrackBlocks,
		UnknownTrackNumbers: scanRes.UnknownTrackNumbers,
		Skipped:             scanRes.Recovery,
		OutputPath:          outPath,
		OutputSize:          sum.Size,
		OutputSHA256:        sha,
		Manifest:            sum.Manifest,
		Verification:        vr,
		Rates:               vr.ComputeRates(),
		Queue:               q.Stats(),
		MappingDigest:       joinDigests(digestParts),
		Timebase:            tb,
		Index:               ix,
		RTPPackets:          rtpPackets,
	}
	for _, s := range rep.Skipped {
		rep.SkippedBytes += s.Length
	}
	return rep, nil
}

func annotate(summaries []track.TrackSummary, stats map[uint64]*demux.TrackStats) []track.TrackSummary {
	out := make([]track.TrackSummary, len(summaries))
	copy(out, summaries)
	for i := range out {
		if st, ok := stats[out[i].Number]; ok && st != nil {
			out[i].Blocks = st.Blocks
			out[i].Frames = st.Frames
			out[i].FirstTick = st.FirstTick
			out[i].LastTick = st.LastTick
			out[i].Keyframes = st.Keyframes
		}
	}
	return out
}

func joinDigests(parts []string) string {
	sort.Strings(parts)
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fault.New(fault.StageVerify, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fault.New(fault.StageVerify, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// classifyRecovery converts skipped damage into a successful outcome when
// recovery was requested, and attaches the skip list to the error otherwise.
func classifyRecovery(err error, skips []fault.Skip, recovery bool) error {
	var fe *fault.Error
	if !errors.As(err, &fe) {
		return err
	}
	if recovery && len(skips) > 0 {
		return &fault.RecoveryError{Cause: err, Skips: skips}
	}
	return err
}
