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
	p.cp.SamplesIn = seen
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
		p.cp.SamplesIn = consumed
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
	p.cp.Cursor = p.cp.SamplesIn
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

func RTPTimestamp(frame int64, rate int) uint32 { return 0 }
func RTPSequence(block int) uint16              { return 0 }
