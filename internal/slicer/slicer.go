package slicer

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"m3u8ctl/internal/ts"
)

// Rendition describes one synthetic rendition.
type Rendition struct {
	Name        string
	Resolution  string
	Bitrate     int
	StartOffset time.Duration
}

// Config controls synthetic segment generation.
type Config struct {
	OutputDir      string
	Segments       int
	TargetDuration time.Duration
	FrameRate      int
	Renditions     []Rendition
	Concurrency    int
	Seed           int64
}

// GeneratedSegment describes a segment written by Generate.
type GeneratedSegment struct {
	Rendition string
	Sequence  int
	Path      string
	Size      int64
	FirstPCR  int64
	Duration  time.Duration
}

// DefaultRenditions returns up to three standard renditions.
func DefaultRenditions(count int) ([]Rendition, error) {
	if count < 1 || count > 3 {
		return nil, fmt.Errorf("renditions must be between 1 and 3")
	}
	all := []Rendition{
		{Name: "1080p", Resolution: "1920x1080", Bitrate: 480000},
		{Name: "720p", Resolution: "1280x720", Bitrate: 300000, StartOffset: 40 * time.Millisecond},
		{Name: "480p", Resolution: "854x480", Bitrate: 180000, StartOffset: -40 * time.Millisecond},
	}
	return append([]Rendition(nil), all[:count]...), nil
}

// Generate synthesizes GOP-aligned TS segments concurrently.
func Generate(cfg Config) ([]GeneratedSegment, error) {
	if cfg.OutputDir == "" {
		return nil, fmt.Errorf("output directory is required")
	}
	if cfg.Segments <= 0 {
		return nil, fmt.Errorf("segments must be positive")
	}
	if cfg.TargetDuration <= 0 {
		return nil, fmt.Errorf("target duration must be positive")
	}
	if cfg.FrameRate <= 0 {
		cfg.FrameRate = 30
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if len(cfg.Renditions) == 0 {
		renditions, err := DefaultRenditions(3)
		if err != nil {
			return nil, err
		}
		cfg.Renditions = renditions
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return nil, err
	}
	ctx := context.Background()
	group, ctx := errgroup.WithContext(ctx)
	sem := semaphore.NewWeighted(int64(cfg.Concurrency))
	var mu sync.Mutex
	var generated []GeneratedSegment
	for _, rendition := range cfg.Renditions {
		rendition := rendition
		if rendition.Name == "" || rendition.Bitrate <= 0 {
			return nil, fmt.Errorf("invalid rendition %q", rendition.Name)
		}
		if err := os.MkdirAll(filepath.Join(cfg.OutputDir, rendition.Name), 0o755); err != nil {
			return nil, err
		}
		for seq := 0; seq < cfg.Segments; seq++ {
			seq := seq
			group.Go(func() error {
				if err := sem.Acquire(ctx, 1); err != nil {
					return err
				}
				defer sem.Release(1)
				seg, err := writeSegment(cfg, rendition, seq)
				if err != nil {
					return err
				}
				mu.Lock()
				generated = append(generated, seg)
				mu.Unlock()
				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return generated, nil
}

func writeSegment(cfg Config, rendition Rendition, seq int) (GeneratedSegment, error) {
	name := fmt.Sprintf("%06d.ts", seq)
	path := filepath.Join(cfg.OutputDir, rendition.Name, name)
	f, err := os.Create(path)
	if err != nil {
		return GeneratedSegment{}, err
	}
	defer f.Close()
	frames := int(math.Round(cfg.TargetDuration.Seconds() * float64(cfg.FrameRate)))
	if frames < 1 {
		frames = 1
	}
	targetBytes := int64(rendition.Bitrate) * int64(cfg.TargetDuration) / int64(8*time.Second)
	packetsPerFrame := int(math.Ceil(float64(targetBytes) / float64(frames*ts.PacketSize)))
	if packetsPerFrame < 1 {
		packetsPerFrame = 1
	}
	startTicks := 10*ts.PCRClock + ts.DurationToTicks(rendition.StartOffset) + ts.DurationToTicks(time.Duration(seq)*cfg.TargetDuration)
	frameTicks := ts.DurationToTicks(time.Second / time.Duration(cfg.FrameRate))
	packet := make([]byte, ts.PacketSize)
	var written int64
	for frame := 0; frame < frames; frame++ {
		for n := 0; n < packetsPerFrame; n++ {
			clear(packet)
			packet[0] = 0x47
			packet[1] = 0x01
			packet[2] = 0x00
			packet[3] = 0x30 | byte((frame*packetsPerFrame+n)&0x0f)
			if n == 0 {
				ticks := startTicks + int64(frame)*frameTicks
				adaptationWithPCR(packet, ticks, frame == 0)
			} else {
				adaptationPlain(packet)
			}
			offset := 13
			if n != 0 {
				offset = 6
			}
			for i := offset; i < ts.PacketSize; i++ {
				packet[i] = byte(i + frame + n + seq + int(cfg.Seed))
			}
			if n == 0 && frame == 0 {
				copy(packet[offset:], []byte{0x00, 0x00, 0x00, 0x01, 0x65})
			}
			if _, err := f.Write(packet); err != nil {
				return GeneratedSegment{}, err
			}
			written += ts.PacketSize
		}
	}
	first := startTicks
	duration := cfg.TargetDuration
	return GeneratedSegment{
		Rendition: rendition.Name, Sequence: seq, Path: path, Size: written,
		FirstPCR: first, Duration: duration,
	}, nil
}

func adaptationWithPCR(packet []byte, ticks int64, randomAccess bool) {
	packet[4] = 7
	packet[5] = 0x10
	if randomAccess {
		packet[5] |= 0x40
	}
	base := uint64(ticks / 300)
	ext := uint16(ticks % 300)
	packet[6] = byte(base >> 25)
	packet[7] = byte(base >> 17)
	packet[8] = byte(base >> 9)
	packet[9] = byte(base >> 1)
	packet[10] = byte(base&0x01)<<7 | byte(ext>>8)
	packet[11] = byte(ext)
}

func adaptationPlain(packet []byte) {
	packet[4] = 1
	packet[5] = 0
}
