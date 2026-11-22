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
