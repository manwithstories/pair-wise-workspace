package pack

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/clearliu/mediq/internal/core"
)

// PCMStage appends interleaved S16LE bytes at a checkpoint-owned offset.
type PCMStage struct {
	Path     string
	Channels int
}

// ByteOffset converts output frames to interleaved PCM bytes.
func (s *PCMStage) ByteOffset(frames int64) int64 {
	return frames * int64(s.Channels) * int64(core.OutputBytesPerSample)
}

// AppendAt truncates any bytes beyond committedFrames and appends chunk at
// the deterministic checkpoint offset. Repeating the same call is safe.
func (s *PCMStage) AppendAt(committedFrames int64, chunk []byte) error {
	if s.Channels <= 0 {
		s.Channels = core.OutputChannels
	}
	blockAlign := s.Channels * core.OutputBytesPerSample
	if len(chunk)%blockAlign != 0 {
		return core.New(core.KindConfig, "append-pcm",
			fmt.Errorf("chunk bytes %d not divisible by %d", len(chunk), blockAlign))
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return core.New(core.KindIO, "create-pcm-dir", err)
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return core.New(core.KindIO, "open-pcm-stage", err)
	}
	defer f.Close()

	offset := s.ByteOffset(committedFrames)
	info, err := f.Stat()
	if err != nil {
		return core.New(core.KindIO, "stat-pcm-stage", err)
	}
	if info.Size() < offset {
		return core.New(core.KindCheckpoint, "pcm-stage-short",
			fmt.Errorf("stage size %d < checkpoint offset %d", info.Size(), offset))
	}
	if info.Size() > offset {
		// A crash may have left bytes that the checkpoint does not cover.
		if err := f.Truncate(offset); err != nil {
			return core.New(core.KindIO, "truncate-pcm-stage", err)
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return core.New(core.KindIO, "seek-pcm-stage", err)
	}
	if _, err := f.Write(chunk); err != nil {
		return core.New(core.KindIO, "append-pcm-stage", err)
	}
	if err := f.Sync(); err != nil {
		return core.New(core.KindIO, "sync-pcm-stage", err)
	}
	return nil
}

// ReadAll returns the staged PCM bytes.
func (s *PCMStage) ReadAll() ([]byte, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, core.New(core.KindIO, "read-pcm-stage", err)
	}
	return b, nil
}
