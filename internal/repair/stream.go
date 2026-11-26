package repair

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"archivectl/internal/core"
	"archivectl/internal/index"
	"archivectl/internal/verify"
)

const streamChunkFrames = 4096

// Options configures one staged repair execution.
type Options struct {
	Index      *index.Index
	WAVPath    string
	ClockPath  string
	OutputPath string

	// AfterWriteBeforeRevalidate is a deterministic test hook used to model a
	// concurrent input change after output bytes have been generated but before
	// the post-execution digest check and atomic commit.
	AfterWriteBeforeRevalidate func() error
}

// Result summarizes a committed repair.
type Result struct {
	OutputPath      string      `json:"output_path"`
	RecoveredFrames int64       `json:"recovered_frames"`
	ZeroedFrames    int64       `json:"zeroed_frames"`
	DamageSpans     int         `json:"damage_spans"`
	Coverage        []core.Span `json:"coverage"`
}

// Run validates the index/input digests, streams PCM through bounded buffers,
// zero-fills indexed damage ranges, and commits output atomically.
func Run(opts Options) (*Result, error) {
	const op = "repair.Run"
	if opts.Index == nil {
		return nil, core.New(core.KindInvalidArgument, op, "index is required")
	}
	if opts.OutputPath == "" {
		return nil, core.New(core.KindInvalidArgument, op, "output path is required")
	}
	wavPath := opts.WAVPath
	if wavPath == "" {
		wavPath = opts.Index.WAVPath
	}
	clockPath := opts.ClockPath
	if clockPath == "" {
		clockPath = opts.Index.ClockPath
	}
	if err := opts.Index.ValidateInputs(wavPath, clockPath); err != nil {
		return nil, err
	}
	preDigest, err := verify.FileSHA256(wavPath)
	if err != nil {
		return nil, err
	}

	cov, err := NewCoverage(opts.Index.Damage)
	if err != nil {
		return nil, err
	}
	if opts.Index.ExpectedFrames <= 0 {
		return nil, core.New(core.KindInvalidArgument, op, "index has no expected frame count")
	}
	if _, err := core.CheckFrameCache(op, streamChunkFrames, opts.Index.Channels, opts.Index.BitsPerSample/8); err != nil {
		return nil, err
	}

	in, err := os.Open(wavPath)
	if err != nil {
		return nil, core.Wrap(core.KindIO, op, wavPath, err)
	}
	defer in.Close()
	dec := wav.NewDecoder(in)
	if err := dec.FwdToPCM(); err != nil {
		return nil, core.Wrap(core.KindMalformedFMT, op, wavPath, err)
	}

	if dir := filepath.Dir(opts.OutputPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(opts.OutputPath), ".repair-*.wav.tmp")
	if err != nil {
		return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	enc := wav.NewEncoder(tmp, opts.Index.SampleRate, opts.Index.BitsPerSample, opts.Index.Channels, 1)
	format := &audio.Format{NumChannels: opts.Index.Channels, SampleRate: opts.Index.SampleRate}
	var zeroed int64
	for start := int64(0); start < opts.Index.ExpectedFrames; start += streamChunkFrames {
		frames := int(opts.Index.ExpectedFrames - start)
		if frames > streamChunkFrames {
			frames = streamChunkFrames
		}
		samples := frames * opts.Index.Channels
		buf := &audio.IntBuffer{
			Format:         format,
			SourceBitDepth: opts.Index.BitsPerSample,
			Data:           make([]int, samples),
		}
		n, decErr := dec.PCMBuffer(buf)
		if decErr != nil && !errors.Is(decErr, io.EOF) && !errors.Is(decErr, io.ErrUnexpectedEOF) {
			_ = enc.Close()
			_ = tmp.Close()
			return nil, core.Wrap(core.KindIO, op, wavPath, decErr)
		}
		validSamples := (n / opts.Index.Channels) * opts.Index.Channels
		if validSamples > len(buf.Data) {
			validSamples = len(buf.Data)
		}
		validFrames := validSamples / opts.Index.Channels
		zeroed += cov.ZeroBuffer(buf.Data[:validSamples], opts.Index.Channels, start, validFrames)
		buf.Data = buf.Data[:validSamples]
		if err := enc.Write(buf); err != nil {
			_ = enc.Close()
			_ = tmp.Close()
			return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
		}

		// PCMBuffer can return fewer samples after tail truncation. Encode the
		// missing frame count as an explicit zero buffer for exact duration.
		if validFrames < frames {
			missing := frames - validFrames
			zeroBuf := &audio.IntBuffer{
				Format:         format,
				SourceBitDepth: opts.Index.BitsPerSample,
				Data:           make([]int, missing*opts.Index.Channels),
			}
			if err := enc.Write(zeroBuf); err != nil {
				_ = enc.Close()
				_ = tmp.Close()
				return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
			}
			zeroed += int64(missing)
		}
	}
	if err := enc.Close(); err != nil {
		_ = tmp.Close()
		return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
	}

	if opts.AfterWriteBeforeRevalidate != nil {
		if err := opts.AfterWriteBeforeRevalidate(); err != nil {
			return nil, err
		}
	}
	postDigest, err := verify.FileSHA256(wavPath)
	if err != nil {
		return nil, err
	}
	if postDigest != preDigest || postDigest != opts.Index.WAVSHA256 {
		return nil, core.New(core.KindInputRace, op,
			"input WAV changed during scan/repair; output was not committed")
	}
	if clockPath != "" {
		postClock, err := verify.FileSHA256(clockPath)
		if err != nil {
			return nil, err
		}
		if postClock != opts.Index.ClockSHA256 {
			return nil, core.New(core.KindInputRace, op,
				"clock record changed during repair; output was not committed")
		}
	}
	if opts.Index.ChecksPath != "" {
		postChecks, err := verify.FileSHA256(opts.Index.ChecksPath)
		if err != nil {
			return nil, err
		}
		if postChecks != opts.Index.ChecksSHA256 {
			return nil, core.New(core.KindInputRace, op,
				"block check record changed during repair; output was not committed")
		}
	}
	if err := os.Rename(tmpName, opts.OutputPath); err != nil {
		return nil, core.Wrap(core.KindIO, op, opts.OutputPath, err)
	}
	removeTemp = false
	return &Result{
		OutputPath:      opts.OutputPath,
		RecoveredFrames: cov.Frames(),
		ZeroedFrames:    zeroed,
		DamageSpans:     len(cov.Spans()),
		Coverage:        cov.Spans(),
	}, nil
}
