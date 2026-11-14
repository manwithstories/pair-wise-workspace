package fixture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"m3u8ctl/internal/slicer"
	"m3u8ctl/internal/ts"
)

// Options controls fixture generation.
type Options struct {
	Out        string
	Scenario   string
	Renditions int
	Segments   int
	Seed       int64
}

// Generate creates a fresh live directory under Out.
func Generate(opt Options) error {
	if opt.Out == "" {
		return fmt.Errorf("output directory is required")
	}
	if opt.Segments <= 0 {
		return fmt.Errorf("segments must be positive")
	}
	switch opt.Scenario {
	case "clean", "gap-reorder", "dup", "truncated", "empty", "pcr-rollback":
	default:
		return fmt.Errorf("unknown scenario %q", opt.Scenario)
	}
	if opt.Scenario == "gap-reorder" && opt.Segments < 10 {
		return fmt.Errorf("gap-reorder requires at least 10 segments")
	}
	renditions, err := slicer.DefaultRenditions(opt.Renditions)
	if err != nil {
		return err
	}
	liveDir := filepath.Join(opt.Out, "live")
	if err := os.RemoveAll(liveDir); err != nil {
		return err
	}
	_, err = slicer.Generate(slicer.Config{
		OutputDir:      liveDir,
		Segments:       opt.Segments,
		TargetDuration: 2 * time.Second,
		FrameRate:      30,
		Renditions:     renditions,
		Concurrency:    4,
		Seed:           opt.Seed,
	})
	if err != nil {
		return err
	}
	return inject(liveDir, renditions, opt)
}

func inject(liveDir string, renditions []slicer.Rendition, opt Options) error {
	seq := func(offset int) int {
		v := opt.Segments - offset
		if v < 0 {
			return 0
		}
		return v
	}
	switch opt.Scenario {
	case "clean":
		return nil
	case "gap-reorder":
		stretch := seq(3)
		for _, rendition := range renditions {
			path := segmentPath(liveDir, rendition.Name, stretch)
			if err := appendFrames(path, 10, 30); err != nil {
				return err
			}
			for n := stretch + 1; n < opt.Segments; n++ {
				if err := shiftPCR(segmentPath(liveDir, rendition.Name, n), 10*ts.PCRClock/30); err != nil {
					return err
				}
			}
		}
		dir := filepath.Join(liveDir, "1080p")
		if err := os.Remove(segmentPath(liveDir, "1080p", seq(9))); err != nil {
			return err
		}
		dupPath := filepath.Join(dir, fmt.Sprintf("%06d-dup.ts", seq(6)))
		if err := copyFile(segmentPath(liveDir, "1080p", seq(6)), dupPath); err != nil {
			return err
		}
		left := segmentPath(liveDir, "1080p", seq(5))
		right := segmentPath(liveDir, "1080p", seq(4))
		if err := os.Rename(left, filepath.Join(dir, fmt.Sprintf("%06d-r.ts", seq(4)))); err != nil {
			return err
		}
		if err := os.Rename(right, filepath.Join(dir, fmt.Sprintf("%06d-r.ts", seq(5)))); err != nil {
			return err
		}
		return nil
	case "dup":
		for _, rendition := range renditions {
			src := segmentPath(liveDir, rendition.Name, seq(5))
			dst := filepath.Join(liveDir, rendition.Name, fmt.Sprintf("%06d-dup.ts", seq(5)))
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
		return nil
	case "truncated", "empty":
		for _, rendition := range renditions {
			path := segmentPath(liveDir, rendition.Name, seq(2))
			if opt.Scenario == "empty" {
				if err := os.Truncate(path, 0); err != nil {
					return err
				}
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if err := os.Truncate(path, info.Size()-100); err != nil {
				return err
			}
		}
		return nil
	case "pcr-rollback":
		for _, rendition := range renditions {
			if err := shiftPCR(segmentPath(liveDir, rendition.Name, seq(5)), -5*ts.PCRClock); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func segmentPath(root, rendition string, seq int) string {
	return filepath.Join(root, rendition, fmt.Sprintf("%06d.ts", seq))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func appendFrames(path string, count, fps int) error {
	probe, err := ts.ProbeFile(path)
	if err != nil {
		return err
	}
	if !probe.HasPCR {
		return fmt.Errorf("%s has no PCR", path)
	}
	frameTicks := ts.DurationToTicks(time.Second / time.Duration(fps))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	packet := make([]byte, ts.PacketSize)
	for n := 0; n < count; n++ {
		ticks := probe.LastPCR + int64(n+1)*frameTicks
		pcrPacket(packet, ticks, byte(n&0x0f))
		if _, err := f.Write(packet); err != nil {
			return err
		}
	}
	return nil
}

func shiftPCR(path string, delta int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	packet := make([]byte, ts.PacketSize)
	for {
		n, err := io.ReadFull(f, packet)
		if err == io.EOF {
			return nil
		}
		if err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n != ts.PacketSize || packet[0] != 0x47 || (packet[3]>>4)&0x03 < 2 || packet[4] < 7 || packet[5]&0x10 == 0 {
			continue
		}
		b := packet[6:12]
		base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 | uint64(b[3])<<1 | uint64(b[4])>>7
		ext := int64(uint16(b[4]&1)<<8 | uint16(b[5]))
		ticks := int64(base)*300 + ext + delta
		if ticks < 0 {
			return fmt.Errorf("PCR underflow in %s", path)
		}
		base = uint64(ticks / 300)
		ext = ticks % 300
		b[0] = byte(base >> 25)
		b[1] = byte(base >> 17)
		b[2] = byte(base >> 9)
		b[3] = byte(base >> 1)
		b[4] = byte(base&1)<<7 | byte(ext>>8)
		b[5] = byte(ext)
		if _, err := f.Seek(int64(-ts.PacketSize), io.SeekCurrent); err != nil {
			return err
		}
		if _, err := f.Write(packet); err != nil {
			return err
		}
	}
}

func pcrPacket(packet []byte, ticks int64, cc byte) {
	clear(packet)
	packet[0] = 0x47
	packet[3] = 0x30 | cc&0x0f
	packet[4] = 7
	packet[5] = 0x10
	base := uint64(ticks / 300)
	ext := uint16(ticks % 300)
	packet[6] = byte(base >> 25)
	packet[7] = byte(base >> 17)
	packet[8] = byte(base >> 9)
	packet[9] = byte(base >> 1)
	packet[10] = byte(base&1)<<7 | byte(ext>>8)
	packet[11] = byte(ext)
}

// Scenarios returns supported scenario names in stable order.
func Scenarios() []string {
	names := []string{"clean", "gap-reorder", "dup", "truncated", "empty", "pcr-rollback"}
	sort.Strings(names)
	return names
}

// ParseSequence extracts the leading decoded sequence from a segment name.
func ParseSequence(name string) (int64, bool) {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	digits := 0
	for digits < len(base) && base[digits] >= '0' && base[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(base[:digits], 10, 64)
	return value, err == nil
}
