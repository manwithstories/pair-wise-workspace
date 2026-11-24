package mp4

import (
	"bytes"
	"encoding/binary"

	gomp4 "github.com/abema/go-mp4"

	"mediarig/internal/media"
)

func writeMvhd(mw *gomp4.Writer, timescale, duration, nextTrackID uint32) error {
	var p bytes.Buffer
	binary.Write(&p, binary.BigEndian, uint32(0)) // creation
	binary.Write(&p, binary.BigEndian, uint32(0)) // modification
	binary.Write(&p, binary.BigEndian, timescale)
	binary.Write(&p, binary.BigEndian, duration)
	binary.Write(&p, binary.BigEndian, uint32(0x00010000)) // rate 1.0
	binary.Write(&p, binary.BigEndian, uint16(0x0100))     // volume 1.0
	binary.Write(&p, binary.BigEndian, uint16(0))          // reserved
	binary.Write(&p, binary.BigEndian, [2]uint32{})        // reserved
	matrix := [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for _, v := range matrix {
		binary.Write(&p, binary.BigEndian, v)
	}
	binary.Write(&p, binary.BigEndian, [6]int32{}) // pre-defined
	binary.Write(&p, binary.BigEndian, nextTrackID)
	return writeFullBox(mw, "mvhd", 0, [3]byte{}, p.Bytes())
}

func writeTkhd(mw *gomp4.Writer, trackID, duration uint32, width, height uint16) error {
	var p bytes.Buffer
	binary.Write(&p, binary.BigEndian, uint32(0))
	binary.Write(&p, binary.BigEndian, uint32(0))
	binary.Write(&p, binary.BigEndian, trackID)
	binary.Write(&p, binary.BigEndian, uint32(0))
	binary.Write(&p, binary.BigEndian, duration)
	binary.Write(&p, binary.BigEndian, [2]uint32{})
	binary.Write(&p, binary.BigEndian, int16(0)) // layer
	binary.Write(&p, binary.BigEndian, int16(0)) // alternate group
	binary.Write(&p, binary.BigEndian, int16(0)) // volume (0 for video)
	binary.Write(&p, binary.BigEndian, uint16(0))
	matrix := [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for _, v := range matrix {
		binary.Write(&p, binary.BigEndian, v)
	}
	binary.Write(&p, binary.BigEndian, uint32(uint32(width)<<16))
	binary.Write(&p, binary.BigEndian, uint32(uint32(height)<<16))
	return writeFullBox(mw, "tkhd", 0, [3]byte{0, 0, 3}, p.Bytes())
}

func writeMdhd(mw *gomp4.Writer, timescale, duration uint32) error {
	var p bytes.Buffer
	binary.Write(&p, binary.BigEndian, uint32(0))
	binary.Write(&p, binary.BigEndian, uint32(0))
	binary.Write(&p, binary.BigEndian, timescale)
	binary.Write(&p, binary.BigEndian, duration)
	// ISO language "und": 0x55C4? Use well-known undetermined code.
	binary.Write(&p, binary.BigEndian, uint16(0x55C4))
	binary.Write(&p, binary.BigEndian, uint16(0))
	return writeFullBox(mw, "mdhd", 0, [3]byte{}, p.Bytes())
}

func writeHdlr(mw *gomp4.Writer, handler, name string) error {
	var p bytes.Buffer
	binary.Write(&p, binary.BigEndian, uint32(0))
	p.WriteString(handler)
	binary.Write(&p, binary.BigEndian, [3]uint32{})
	p.WriteString(name)
	p.WriteByte(0)
	return writeFullBox(mw, "hdlr", 0, [3]byte{}, p.Bytes())
}

func writeDinf(mw *gomp4.Writer) error {
	return writeContainer(mw, "dinf", func() error {
		var dref bytes.Buffer
		binary.Write(&dref, binary.BigEndian, uint32(1))
		if err := writeFullBox(mw, "dref", 0, [3]byte{}, dref.Bytes()); err != nil {
			return err
		}
		return writeFullBox(mw, "url ", 0, [3]byte{0, 0, 1}, nil)
	})
}

func writeAudioTrak(
	mw *gomp4.Writer,
	cfg MuxConfig,
	durations []uint32,
	sizes []uint32,
	duration uint64,
	chunkOffset uint32,
) error {
	timescale := cfg.Audio.Timescale
	if timescale == 0 {
		timescale = 48000
	}
	trackDuration := uint32(media.SecondsToSamples(
		media.SamplesToSeconds(int64(duration), int(timescale)), 1000))
	return writeContainer(mw, "trak", func() error {
		if err := writeTkhd(mw, 1, trackDuration, 0, 0); err != nil {
			return err
		}
		return writeContainer(mw, "mdia", func() error {
			if err := writeMdhd(mw, timescale, uint32(duration)); err != nil {
				return err
			}
			if err := writeHdlr(mw, "soun", "MediaRig PCM Audio"); err != nil {
				return err
			}
			return writeContainer(mw, "minf", func() error {
				if err := writeFullBox(mw, "smhd", 0, [3]byte{}, be16(0)); err != nil {
					return err
				}
				if err := writeDinf(mw); err != nil {
					return err
				}
				return writeAudioStbl(mw, cfg, durations, sizes, chunkOffset)
			})
		})
	})
}

func writeVideoTrak(
	mw *gomp4.Writer,
	cfg MuxConfig,
	deltas, sizes []uint32,
	sync []uint32,
	duration uint64,
	chunkOffset uint32,
) error {
	timescale := cfg.Video.Timescale
	if timescale == 0 {
		timescale = 90000
	}
	trackDuration := uint32(media.SecondsToSamples(
		media.SamplesToSeconds(int64(duration), int(timescale)), 1000))
	return writeContainer(mw, "trak", func() error {
		if err := writeTkhd(mw, 2, trackDuration, cfg.Video.Width, cfg.Video.Height); err != nil {
			return err
		}
		return writeContainer(mw, "mdia", func() error {
			if err := writeMdhd(mw, timescale, uint32(duration)); err != nil {
				return err
			}
			if err := writeHdlr(mw, "vide", "MediaRig AVC Video"); err != nil {
				return err
			}
			return writeContainer(mw, "minf", func() error {
				if err := writeFullBox(mw, "vmhd", 0, [3]byte{0, 0, 1}, make([]byte, 8)); err != nil {
					return err
				}
				if err := writeDinf(mw); err != nil {
					return err
				}
				return writeVideoStbl(mw, cfg, deltas, sizes, sync, chunkOffset)
			})
		})
	})
}
