package demux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
)

// Open demuxes an MP4/MOV file. Media errors are accumulated in
// Container.Faults; only I/O and argument problems are returned as errors.
func Open(path string) (*Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	c := &Container{Path: path, Size: st.Size(), Faults: &fault.List{}}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}
	c.Fingerprint = hex.EncodeToString(h.Sum(nil))
	if st.Size() == 0 {
		c.Faults.Add(fault.Fatal, 0, 0, -1, "empty file")
		return c, nil
	}
	var moov *boxHeader
	walkErr := walkBoxes(f, 0, st.Size(), func(bh boxHeader) error {
		if bh.typ == "moov" {
			b := bh
			moov = &b
		}
		return nil
	})
	if walkErr != nil {
		c.Faults.Add(fault.Skip, st.Size(), 0, -1, "box walk stopped early: %v", walkErr)
	}
	if moov == nil {
		c.Faults.Add(fault.Fatal, 0, 0, -1, "no moov box found; index unrecoverable")
		return c, nil
	}
	moovPayload, err := readPayload(f, *moov, maxMoovBytes)
	if err != nil {
		c.Faults.Add(fault.Fatal, moov.off, 0, -1, "read moov: %v", err)
		return c, nil
	}
	if err := c.parseMoov(bytes.NewReader(moovPayload), int64(len(moovPayload)), f); err != nil {
		c.Faults.Add(fault.Fatal, moov.off, 0, -1, "parse moov: %v", err)
	}
	return c, nil
}

func (c *Container) parseMoov(r io.ReaderAt, moovLen int64, file io.ReaderAt) error {
	return walkBoxes(r, 0, moovLen, func(bh boxHeader) error {
		switch bh.typ {
		case "mvhd":
			var mvhd mp4.Mvhd
			if err := readInto(r, bh, &mvhd); err != nil {
				return err
			}
			c.MovieTimescale = mvhd.Timescale
			c.DurationTicks = clock.Ticks(int64(mvhd.DurationV0), mvhd.Timescale)
		case "trak":
			t, err := c.parseTrak(r, bh, file)
			if err != nil {
				return err
			}
			c.Tracks = append(c.Tracks, t)
		}
		return nil
	})
}
