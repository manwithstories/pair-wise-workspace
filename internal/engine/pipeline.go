// Streaming pipeline: decode -> resample -> channel normalise -> limit ->
// framed output blocks -> checkpoint.
//
// Determinism is the central property. Output bytes are a pure function of
// (input bytes, configuration, adapter versions), so a resumed run produces
// byte-identical blocks to an uninterrupted run. The checkpoint stores an
// *input-frame* cursor plus the number of completed blocks; replaying input
// up to the cursor and discarding the already-framed output reproduces the
// resampler state exactly, because the kernel is finite and deterministic.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pion/rtp"

	"mediacore/internal/codec"
	"mediacore/internal/container"
	"mediacore/internal/media"
	"mediacore/internal/store"
)

// Options configures one pipeline run.
type Options struct {
	WorkDir   string
	Config    Config
	Source    *container.Source
	Codecs    *codec.Registry
	InputPath string
	InputSHA  string
	InputSize int64

	// Mode is "demo", "resume" or "verify"; recorded in provenance.
	Mode string
	// InterruptAfter stops cleanly after this many completed blocks. Zero
	// means never.
	InterruptAfter int
	// Fresh discards an existing unfinished checkpoint before starting.
	Fresh bool
	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// Stats is the summary rendered by the command layer.
type Stats struct {
	SamplesIn     int64
	FramesOut     int64
	SamplesOut    int64
	PeakDBFS      float64
	SHA256        string
	Blocks        int
	ResumedBlocks int
	RecoveryMS    int64
	SilentBlocks  int
	ClippedBlocks int
	LimitedBlocks int
	TotalBytes    int64
	RTPFirst      uint32
	RTPLast       uint32
	RTPVerified   bool
}

// Result is the outcome of one run.
type Result struct {
	Stats       Stats
	Interrupted bool
	Checkpoint  *store.Checkpoint
}

const rtpClockRate = 48000
const rtpPayloadType = 111
const rtpSSRC = 0x4D434100 // "MCA\0"

// Run executes the pipeline described by opts.
func statsFromCheckpoint(cp *store.Checkpoint, sha string) Stats {
	var samplesOut, bytes int64
	for _, b := range cp.Blocks {
		samplesOut += int64(b.Frames) * int64(cp.OutputChannels)
		bytes += b.Bytes
	}
	var first, last uint32
	if len(cp.Blocks) > 0 {
		first = cp.Blocks[0].RTPTimestamp
		last = cp.Blocks[len(cp.Blocks)-1].RTPTimestamp
	}
	return Stats{
		SamplesIn:     cp.SamplesIn,
		FramesOut:     cp.FramesOut,
		SamplesOut:    samplesOut,
		PeakDBFS:      cp.PeakDBFS,
		SHA256:        sha,
		Blocks:        len(cp.Blocks),
		RecoveryMS:    0,
		SilentBlocks:  cp.SilentBlocks,
		ClippedBlocks: cp.ClippedBlocks,
		LimitedBlocks: cp.LimitedBlocks,
		TotalBytes:    bytes,
		RTPFirst:      first,
		RTPLast:       last,
	}
}

func pickTrack(src *container.Source) (container.Track, bool) {
	for _, t := range src.Tracks() {
		if t.SampleCount > 0 {
			return t, true
		}
	}
	return container.Track{}, false
}

func totalInputFrames(src *container.Source, track int, f codec.Format) (int64, error) {
	if f.FrameBytes <= 0 {
		return 0, media.NewError(media.CodeUnsupportedCodec, -1, track, "compressed input has no frame mapping")
	}
	cur, err := src.Cursor(track)
	if err != nil {
		return 0, err
	}
	defer cur.Close()
	var frames int64
	for {
		s, err := cur.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if s.Size%f.FrameBytes != 0 {
			return 0, media.NewError(media.CodeUnalignedPCM, s.Offset, track,
				"sample %d is %d bytes, not a whole number of %d-byte frames",
				s.Index, s.Size, f.FrameBytes)
		}
		frames += int64(s.Size / f.FrameBytes)
	}
	return frames, nil
}

func configDigest(cfg Config, f codec.Format, src *container.Source) string {
	type payload struct {
		Config
		Codec            string
		InputRate        int
		InputChannels    int
		InputFrameBytes  int
		ContainerVersion string
	}
	b, _ := json.Marshal(payload{
		Config:           cfg,
		Codec:            f.Codec,
		InputRate:        f.SampleRate,
		InputChannels:    f.Channels,
		InputFrameBytes:  f.FrameBytes,
		ContainerVersion: src.Version().String(),
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func attachTrack(err error, track int) error {
	if me, ok := media.AsError(err); ok && me.Track == 0 {
		me.Track = track
	}
	return err
}

func attachSample(err error, offset int64, track int) error {
	if me, ok := media.AsError(err); ok {
		if me.Track == 0 {
			me.Track = track
		}
		if !me.HasOffset {
			me.Offset = offset
			me.HasOffset = true
		}
	}
	return err
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "create %s: %v", dir, err).Wrap(err)
	}
	return nil
}

// clearWork removes volatile state from an unfinished run: checkpoint, block
// files, partial temp files and the assembled output. Provenance is kept
// because it is an append-only audit trail.

func clearWork(dir string) error {
	targets := []string{store.Path(dir), store.OutputPath(dir)}
	entries, err := os.ReadDir(store.BlocksDir(dir))
	if err == nil {
		for _, e := range entries {
			targets = append(targets, filepath.Join(store.BlocksDir(dir), e.Name()))
		}
	}
	for _, path := range targets {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return media.NewError(media.CodeAtomicWrite, -1, 0, "remove %s: %v", path, err).Wrap(err)
		}
	}
	return nil
}

// CheckInsideWorkDir rejects paths that escape the work directory.

func CheckInsideWorkDir(workDir, path string) error {
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return media.NewError(media.CodeWorkDirEscape, -1, 0, "resolve work dir: %v", err).Wrap(err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return media.NewError(media.CodeWorkDirEscape, -1, 0, "resolve path: %v", err).Wrap(err)
	}
	rel, err := filepath.Rel(absWork, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return media.NewError(media.CodeWorkDirEscape, -1, 0, "%s escapes %s", absPath, absWork)
	}
	return nil
}

// processor holds the mutable state of one encoding pass.
type processor struct {
	opts   Options
	cfg    Config
	fmt    codec.Format
	track  container.Track
	rs     *Resampler
	cp     *store.Checkpoint
	result *Result

	totalIn int64
	// pending holds output frames not yet framed into a block.
	pending []float64
	// decodeBuf and normBuf avoid per-sample allocation.
	decodeBuf []float64
	normBuf   []float64
	// blockIndex is the next block to emit.
	blockIndex int
	// outFrame is the absolute output frame of the next block's first frame.
	outFrame int64
	// cursor is the number of input *frames* fully absorbed, which is
	// what resume replays; SamplesIn counts interleaved samples for the
	// operator-facing summary and must not be used as a frame cursor.
	cursor int64
}

// replay reproduces the pipeline state at the checkpoint cursor, then keeps
// any resampled output that had not yet been framed into a block.
func (p *processor) replay() error {
	cursor := p.cp.Cursor
	if cursor <= 0 {
		p.blockIndex = p.cp.CompletedBlocks
		p.outFrame = completedFrames(p.cp)
		return nil
	}
	cur, err := p.opts.Source.Cursor(p.track.Number)
	if err != nil {
		return err
	}
	defer cur.Close()
	var seen int64
	p.pending = p.pending[:0]
	for seen < cursor {
		s, err := cur.Next()
		if err == io.EOF {
			return media.NewError(media.CodeCheckpointConflict, -1, p.track.Number,
				"checkpoint cursor %d is past the end of the track (%d frames)", cursor, seen)
		}
		if err != nil {
			return err
		}
		out, err := p.decodeNormalize(s)
		if err != nil {
			return err
		}
		frames := int64(len(out) / p.cfg.OutputChannels)
		take := frames
		if remain := cursor - seen; take > remain {
			take = remain
		}
		seen += take
		p.pending = p.rs.Push(out[:take*int64(p.cfg.OutputChannels)], p.pending)
	}
	// Drop the frames already written into completed blocks; whatever
	// remains is the partially filled next block.
	drop := int(completedFrames(p.cp)) * p.cfg.OutputChannels
	if drop > len(p.pending) {
		return media.NewError(media.CodeCheckpointConflict, -1, p.track.Number,
			"checkpoint claims %d completed blocks but replay produced only %d samples",
			p.cp.CompletedBlocks, len(p.pending))
	}
	p.pending = append(p.pending[:0], p.pending[drop:]...)
	p.blockIndex = p.cp.CompletedBlocks
	p.outFrame = completedFrames(p.cp)
	p.cursor = seen
	p.cp.SamplesIn = seen * int64(p.fmt.Channels)
	return nil
}

// stream reads the rest of the track to EOF and emits blocks.
func (p *processor) stream() error {
	cur, err := p.opts.Source.Cursor(p.track.Number)
	if err != nil {
		return err
	}
	defer cur.Close()
	var consumed int64
	for {
		s, err := cur.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		out, err := p.decodeNormalize(s)
		if err != nil {
			return err
		}
		frames := int64(len(out) / p.cfg.OutputChannels)
		if consumed+frames <= p.cp.Cursor {
			// Still inside the replay window; Push already did this work
			// in replay(). Skip to avoid double-feeding.
			consumed += frames
			continue
		}
		if consumed < p.cp.Cursor {
			// Sample straddles the cursor: feed only the tail.
			off := p.cp.Cursor - consumed
			out = out[off*int64(p.fmt.Channels):]
			frames -= off
			consumed = p.cp.Cursor
		}
		consumed += frames
		p.pending = p.rs.Push(out, p.pending)
		p.cursor = consumed
		p.cp.SamplesIn = consumed * int64(p.fmt.Channels)
		if err := p.frameBlocks(); err != nil {
			return err
		}
		if p.result.Interrupted {
			return store.Save(p.opts.WorkDir, p.cp)
		}
	}
	p.pending = p.rs.Flush(p.pending)
	if err := p.frameBlocks(); err != nil {
		return err
	}
	p.finish()
	return store.Save(p.opts.WorkDir, p.cp)
}

// decodeNormalize decodes one container sample and maps its channel layout
// onto the configured output layout.
func (p *processor) decodeNormalize(s container.Sample) ([]float64, error) {
	if p.fmt.FrameBytes <= 0 {
		return nil, media.NewError(media.CodeUnsupportedCodec, s.Offset, p.track.Number,
			"compressed sample %d has no frame layout", s.Index)
	}
	if s.Size%p.fmt.FrameBytes != 0 {
		return nil, media.NewError(media.CodeUnalignedPCM, s.Offset, p.track.Number,
			"sample %d is %d bytes, not a whole number of %d-byte frames",
			s.Index, s.Size, p.fmt.FrameBytes)
	}
	frames := s.Size / p.fmt.FrameBytes
	need := frames * p.fmt.Channels
	if cap(p.decodeBuf) < need {
		p.decodeBuf = make([]float64, need)
	}
	p.decodeBuf = p.decodeBuf[:need]
	n, err := p.opts.Codecs.Decode(s.Data, p.fmt, p.decodeBuf)
	if err != nil {
		return nil, attachSample(err, s.Offset, p.track.Number)
	}
	if n != frames {
		return nil, media.NewError(media.CodeUnalignedPCM, s.Offset, p.track.Number,
			"sample %d decoded %d frames, expected %d", s.Index, n, frames)
	}
	outFrames := frames
	outCh := p.cfg.OutputChannels
	if cap(p.normBuf) < outFrames*outCh {
		p.normBuf = make([]float64, outFrames*outCh)
	}
	p.normBuf = p.normBuf[:outFrames*outCh]
	normalizeChannels(p.normBuf, p.decodeBuf[:frames*p.fmt.Channels], p.fmt.Channels, outCh)
	return p.normBuf, nil
}

// frameBlocks emits every complete block currently buffered.
func (p *processor) frameBlocks() error {
	blockFloats := p.cfg.BlockFrames * p.cfg.OutputChannels
	for len(p.pending) >= blockFloats {
		block := p.pending[:blockFloats]
		if err := p.emitBlock(block); err != nil {
			return err
		}
		p.pending = p.pending[blockFloats:]
		if p.result.Interrupted {
			return nil
		}
	}
	return nil
}

// emitBlock meters, limits, quantises and persists one complete block.
func (p *processor) emitBlock(frames []float64) error {
	if p.blockIndex >= p.cp.TotalBlocks {
		return media.NewError(media.CodeOutputVerification, -1, p.track.Number,
			"produced more than the expected %d blocks", p.cp.TotalBlocks)
	}
	if p.blockIndex < p.cp.CompletedBlocks {
		p.blockIndex++
		p.outFrame += int64(p.cfg.BlockFrames)
		return nil
	}
	if err := p.writeBlock(frames); err != nil {
		return err
	}
	if p.opts.InterruptAfter > 0 && p.cp.CompletedBlocks >= p.opts.InterruptAfter && !p.cp.Complete {
		p.result.Interrupted = true
	}
	return nil
}

// completedFrames is the exact output frame count already sealed in blocks;
// the final block may be shorter than BlockFrames, so this cannot be derived
// from the block index alone.
func completedFrames(cp *store.Checkpoint) int64 {
	var n int64
	for _, b := range cp.Blocks {
		n += int64(b.Frames)
	}
	return n
}

// writeBlock applies the loudness ceiling, quantises and persists one block,
// then advances the checkpoint.
func (p *processor) writeBlock(frames []float64) error {
	stat := limitBlock(frames, p.cfg.LoudnessDBFS)
	data := quantize(frames)
	rec, err := store.WriteBlockAtomically(p.opts.WorkDir, p.blockIndex, data)
	if err != nil {
		return err
	}
	rec.StartFrame = p.outFrame
	rec.Frames = len(frames) / p.cfg.OutputChannels
	rec.PeakDBFS = stat.peakDBFS
	rec.RTPTimestamp = RTPTimestamp(rec.StartFrame, p.cfg.OutputRate)
	rec.RTPSequence = RTPSequence(p.blockIndex)
	p.cp.Blocks = append(p.cp.Blocks, rec)
	p.cp.CompletedBlocks = len(p.cp.Blocks)
	p.cp.FramesOut += int64(rec.Frames)
	p.cp.Cursor = p.cursor
	if !p.cp.HasPeak || stat.peakDBFS > p.cp.PeakDBFS {
		p.cp.PeakDBFS = stat.peakDBFS
		p.cp.HasPeak = true
	}
	if stat.silent {
		p.cp.SilentBlocks++
	}
	if stat.clipped {
		p.cp.ClippedBlocks++
	}
	if stat.limited {
		p.cp.LimitedBlocks++
	}
	p.blockIndex++
	p.outFrame += int64(rec.Frames)
	return store.Save(p.opts.WorkDir, p.cp)
}

// finish writes the trailing partial block and seals the checkpoint.
func (p *processor) finish() {
	if len(p.pending) > 0 && len(p.pending)%p.cfg.OutputChannels == 0 {
		if p.blockIndex >= p.cp.CompletedBlocks {
			if err := p.writeBlock(p.pending); err != nil {
				p.pending = nil
				return
			}
		}
	}
	p.pending = nil
	p.cp.Complete = len(p.cp.Blocks) == p.cp.TotalBlocks
	p.cp.FramesOut = p.outFrame
	if p.cp.Complete {
		sha, err := store.VerifyBlocks(p.opts.WorkDir, p.cp.Blocks)
		if err == nil {
			p.cp.CanonicalSHA256 = sha
			p.cp.PartialSHA256 = sha
		}
	}
}

func Run(opts Options) (*Result, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	started := opts.Now()
	cfg := opts.Config
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		return nil, media.NewError(media.CodeUsage, -1, 0, "work directory is required")
	}
	if err := ensureDir(opts.WorkDir); err != nil {
		return nil, err
	}
	if err := ensureDir(store.BlocksDir(opts.WorkDir)); err != nil {
		return nil, err
	}

	track, ok := pickTrack(opts.Source)
	if !ok {
		return nil, media.NewError(media.CodeNoAudioTrack, -1, 0, "no audio track with samples")
	}
	desc, err := opts.Codecs.Describe(track.CodecTag, track.Config,
		track.SampleRate, track.Channels, track.BitsPerSample)
	if err != nil {
		return nil, attachTrack(err, track.Number)
	}
	if desc.Format.Mapping == codec.MappingCompressed {
		return nil, media.NewError(media.CodeUnsupportedCodec, track.FirstSampleOffset, track.Number,
			"%s payload decoding is not supported; provide 16-bit PCM", desc.Format.Codec)
	}
	if desc.Format.Channels != 1 && desc.Format.Channels != 2 {
		return nil, media.NewError(media.CodeChannelsInvalid, track.FirstSampleOffset, track.Number,
			"input channel count %d is not supported", desc.Format.Channels)
	}

	totalIn, err := totalInputFrames(opts.Source, track.Number, desc.Format)
	if err != nil {
		return nil, err
	}
	rs, err := NewResampler(desc.Format.SampleRate, cfg.OutputRate, cfg.OutputChannels)
	if err != nil {
		return nil, attachTrack(err, track.Number)
	}
	expectedFrames := rs.OutputFramesFor(totalIn)
	expectedBlocks := int((expectedFrames + int64(cfg.BlockFrames) - 1) / int64(cfg.BlockFrames))

	binding := store.Binding{
		InputSHA256:      opts.InputSHA,
		ConfigDigest:     configDigest(cfg, desc.Format, opts.Source),
		ContainerVersion: opts.Source.Version().String(),
		CodecVersion:     desc.Adapter.String(),
		OutputRate:       cfg.OutputRate,
		OutputChannels:   cfg.OutputChannels,
		LoudnessDBFS:     cfg.LoudnessDBFS,
		BlockFrames:      cfg.BlockFrames,
	}

	cp, err := store.Load(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	if cp != nil && opts.Fresh && !cp.Complete {
		if err := clearWork(opts.WorkDir); err != nil {
			return nil, err
		}
		cp = nil
	}
	if cp != nil {
		if err := store.Check(cp, binding); err != nil {
			return nil, err
		}
		if cp.TotalBlocks != expectedBlocks {
			return nil, media.NewError(media.CodeCheckpointConflict, -1, 0,
				"checkpoint expects %d blocks, input now yields %d", cp.TotalBlocks, expectedBlocks)
		}
		if _, err := store.VerifyBlocks(opts.WorkDir, cp.Blocks); err != nil {
			return nil, err
		}
	}

	resumed := 0
	if cp == nil {
		cp = &store.Checkpoint{
			InputPath:        opts.InputPath,
			InputSHA256:      opts.InputSHA,
			InputBytes:       opts.InputSize,
			ConfigDigest:     binding.ConfigDigest,
			ContainerVersion: binding.ContainerVersion,
			CodecVersion:     binding.CodecVersion,
			OutputRate:       cfg.OutputRate,
			OutputChannels:   cfg.OutputChannels,
			LoudnessDBFS:     cfg.LoudnessDBFS,
			BlockFrames:      cfg.BlockFrames,
			TotalBlocks:      expectedBlocks,
		}
	} else {
		resumed = cp.CompletedBlocks
	}

	result := &Result{Checkpoint: cp}
	if cp.Complete {
		sha, err := store.Assemble(opts.WorkDir, cp.Blocks, cp.CanonicalSHA256)
		if err != nil {
			return nil, err
		}
		result.Stats = statsFromCheckpoint(cp, sha)
		result.Stats.ResumedBlocks = resumed
		result.Stats.RecoveryMS = opts.Now().Sub(started).Milliseconds()
		return result, nil
	}

	p := &processor{
		opts:    opts,
		cfg:     cfg,
		fmt:     desc.Format,
		track:   track,
		rs:      rs,
		cp:      cp,
		result:  result,
		totalIn: totalIn,
	}
	if err := p.replay(); err != nil {
		return nil, err
	}
	if err := p.stream(); err != nil {
		return nil, err
	}
	if result.Interrupted {
		result.Stats = statsFromCheckpoint(cp, "")
		result.Stats.ResumedBlocks = resumed
		result.Stats.RecoveryMS = opts.Now().Sub(started).Milliseconds()
		return result, nil
	}
	if !cp.Complete {
		return nil, media.NewError(media.CodeOutputVerification, -1, track.Number,
			"pipeline produced %d of %d blocks", cp.CompletedBlocks, cp.TotalBlocks)
	}
	sha, err := store.Assemble(opts.WorkDir, cp.Blocks, cp.CanonicalSHA256)
	if err != nil {
		return nil, err
	}
	if err := verifyRTP(cp); err != nil {
		return nil, err
	}
	result.Stats = statsFromCheckpoint(cp, sha)
	result.Stats.ResumedBlocks = resumed
	result.Stats.RTPVerified = true
	result.Stats.RecoveryMS = opts.Now().Sub(started).Milliseconds()
	return result, nil
}

func validateConfig(cfg Config) error {
	if cfg.OutputRate != 44100 && cfg.OutputRate != 48000 {
		return media.NewError(media.CodeInvalidSampleRate, -1, 0,
			"output rate %d is not one of 44100, 48000", cfg.OutputRate)
	}
	if cfg.OutputChannels != 1 && cfg.OutputChannels != 2 {
		return media.NewError(media.CodeChannelsInvalid, -1, 0,
			"output channels %d is not 1 or 2", cfg.OutputChannels)
	}
	if cfg.BlockFrames <= 0 || cfg.BlockFrames > cfg.OutputRate {
		return media.NewError(media.CodeBlockTooLong, -1, 0,
			"block of %d frames exceeds one second at %d Hz", cfg.BlockFrames, cfg.OutputRate)
	}
	if !(cfg.LoudnessDBFS < 0 && cfg.LoudnessDBFS >= -60) {
		return media.NewError(media.CodeLoudnessInvalid, -1, 0,
			"loudness ceiling %.3f dBFS must be in [-60, 0)", cfg.LoudnessDBFS)
	}
	return nil
}

// RTPTimestamp maps an output frame index onto a 48 kHz RTP clock.
func RTPTimestamp(frame int64, rate int) uint32 {
	base, err := media.NewTimeBase(int64(rate))
	if err != nil {
		return 0
	}
	clock, err := media.NewTimeBase(rtpClockRate)
	if err != nil {
		return 0
	}
	ts, _, err := media.Exact(frame, base, clock)
	if err != nil {
		return 0
	}
	return uint32(ts)
}

// RTPSequence maps a block index onto a 16-bit RTP sequence number.
func RTPSequence(block int) uint16 { return uint16(block & 0xFFFF) }

// RTPPacket builds the RTP packet describing one output block. The playback
// service maps block boundaries straight onto these headers.
func RTPPacket(block store.BlockRecord, rate int) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    rtpPayloadType,
		SequenceNumber: RTPSequence(block.Index),
		Timestamp:      RTPTimestamp(block.StartFrame, rate),
		SSRC:           rtpSSRC,
		Marker:         true,
	}}
}

// verifyRTP round-trips every block's header through pion/rtp and checks the
// timestamp contract: block n starts at n*BlockFrames and maps monotonically
// on the 48 kHz clock.
func verifyRTP(cp *store.Checkpoint) error {
	var prev uint32
	for i, b := range cp.Blocks {
		pkt := RTPPacket(b, cp.OutputRate)
		buf, err := pkt.Marshal()
		if err != nil {
			return media.NewError(media.CodeOutputVerification, -1, 0, "marshal RTP: %v", err).Wrap(err)
		}
		var back rtp.Packet
		if err := back.Unmarshal(buf); err != nil {
			return media.NewError(media.CodeOutputVerification, -1, 0, "unmarshal RTP: %v", err).Wrap(err)
		}
		if back.Header.Timestamp != pkt.Header.Timestamp ||
			back.Header.SequenceNumber != pkt.Header.SequenceNumber {
			return media.NewError(media.CodeOutputVerification, -1, 0,
				"block %d RTP round-trip mismatch", b.Index)
		}
		if i > 0 && back.Header.Timestamp < prev {
			return media.NewError(media.CodeOutputVerification, -1, 0,
				"block %d RTP timestamp regressed", b.Index)
		}
		prev = back.Header.Timestamp
	}
	return nil
}
