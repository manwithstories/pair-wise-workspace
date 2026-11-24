package mp4

import (
	"bytes"
	"encoding/binary"

	gomp4 "github.com/abema/go-mp4"

	"mediarig/internal/media"
)

func writeAudioStbl(mw *gomp4.Writer, cfg MuxConfig, durations, sizes []uint32, offset uint32) error {
	return writeContainer(mw, "stbl", func() error {
		if err := writeAudioStsd(mw, cfg); err != nil {
			return err
		}
		if err := writeStts(mw, durations); err != nil {
			return err
		}
		count := uint32(len(cfg.Audio.Samples))
		var stsc bytes.Buffer
		binary.Write(&stsc, binary.BigEndian, uint32(1))
		binary.Write(&stsc, binary.BigEndian, uint32(1))
		binary.Write(&stsc, binary.BigEndian, count)
		binary.Write(&stsc, binary.BigEndian, uint32(1))
		if err := writeFullBox(mw, "stsc", 0, [3]byte{}, stsc.Bytes()); err != nil {
			return err
		}
		if err := writeStsz(mw, sizes); err != nil {
			return err
		}
		var stco bytes.Buffer
		binary.Write(&stco, binary.BigEndian, uint32(1))
		binary.Write(&stco, binary.BigEndian, offset)
		return writeFullBox(mw, "stco", 0, [3]byte{}, stco.Bytes())
	})
}

func writeAudioStsd(mw *gomp4.Writer, cfg MuxConfig) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeStsd()}); err != nil {
		return media.InternalWrap(media.StageMP4, err, "start stsd")
	}
	if _, err := mw.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	if _, err := mw.Write(be32(1)); err != nil {
		return err
	}
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeIpcm()}); err != nil {
		return media.InternalWrap(media.StageMP4, err, "start ipcm")
	}
	ch := cfg.Audio.Channels
	if ch == 0 {
		ch = 2
	}
	bits := cfg.Audio.Bits
	if bits == 0 {
		bits = 16
	}
	rate := cfg.Audio.SampleRate
	if rate == 0 {
		rate = 48000
	}
	var e bytes.Buffer
	e.Write(make([]byte, 6))
	binary.Write(&e, binary.BigEndian, uint16(1)) // data ref
	binary.Write(&e, binary.BigEndian, uint16(0)) // entry version
	binary.Write(&e, binary.BigEndian, [3]uint16{})
	binary.Write(&e, binary.BigEndian, uint16(ch))
	binary.Write(&e, binary.BigEndian, uint16(bits))
	binary.Write(&e, binary.BigEndian, uint16(0))
	binary.Write(&e, binary.BigEndian, uint16(0))
	binary.Write(&e, binary.BigEndian, uint32(rate<<16))
	if _, err := mw.Write(e.Bytes()); err != nil {
		return err
	}
	// format_flags bit 0 marks little-endian PCM; the muxer encodes s16le.
	if err := writeFullBox(mw, "pcmC", 0, [3]byte{}, []byte{0x01, byte(bits)}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // ipcm
		return err
	}
	if _, err := mw.EndBox(); err != nil { // stsd
		return err
	}
	return nil
}

func writeVideoStbl(mw *gomp4.Writer, cfg MuxConfig, deltas, sizes, sync []uint32, offset uint32) error {
	return writeContainer(mw, "stbl", func() error {
		if err := writeVideoStsd(mw, cfg); err != nil {
			return err
		}
		if err := writeStts(mw, deltas); err != nil {
			return err
		}
		count := uint32(len(cfg.Video.Samples))
		var stsc bytes.Buffer
		binary.Write(&stsc, binary.BigEndian, uint32(1))
		binary.Write(&stsc, binary.BigEndian, uint32(1))
		binary.Write(&stsc, binary.BigEndian, count)
		binary.Write(&stsc, binary.BigEndian, uint32(1))
		if err := writeFullBox(mw, "stsc", 0, [3]byte{}, stsc.Bytes()); err != nil {
			return err
		}
		if err := writeStsz(mw, sizes); err != nil {
			return err
		}
		if len(sync) > 0 {
			var stss bytes.Buffer
			binary.Write(&stss, binary.BigEndian, uint32(len(sync)))
			for _, n := range sync {
				binary.Write(&stss, binary.BigEndian, n)
			}
			if err := writeFullBox(mw, "stss", 0, [3]byte{}, stss.Bytes()); err != nil {
				return err
			}
		}
		var stco bytes.Buffer
		binary.Write(&stco, binary.BigEndian, uint32(1))
		binary.Write(&stco, binary.BigEndian, offset)
		return writeFullBox(mw, "stco", 0, [3]byte{}, stco.Bytes())
	})
}

func writeVideoStsd(mw *gomp4.Writer, cfg MuxConfig) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeStsd()}); err != nil {
		return err
	}
	mw.Write([]byte{0, 0, 0, 0})
	mw.Write(be32(1))
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.StrToBoxType("avc1")}); err != nil {
		return err
	}
	var e bytes.Buffer
	e.Write(make([]byte, 6))
	binary.Write(&e, binary.BigEndian, uint16(1))
	binary.Write(&e, binary.BigEndian, uint16(0)) // predefined
	binary.Write(&e, binary.BigEndian, uint16(0)) // reserved
	binary.Write(&e, binary.BigEndian, [3]uint32{})
	binary.Write(&e, binary.BigEndian, cfg.Video.Width)
	binary.Write(&e, binary.BigEndian, cfg.Video.Height)
	binary.Write(&e, binary.BigEndian, uint32(0x00480000))
	binary.Write(&e, binary.BigEndian, uint32(0x00480000))
	binary.Write(&e, binary.BigEndian, uint32(0))
	binary.Write(&e, binary.BigEndian, uint16(1))
	var compressor [32]byte
	compressor[0] = byte(len("MediaRig H264"))
	copy(compressor[1:], "MediaRig H264")
	e.Write(compressor[:])
	binary.Write(&e, binary.BigEndian, uint16(0x0018))
	binary.Write(&e, binary.BigEndian, int16(-1))
	if _, err := mw.Write(e.Bytes()); err != nil {
		return err
	}
	if err := writeRawBox(mw, "avcC", buildAvcC(cfg.Video.SPS, cfg.Video.PPS)); err != nil {
		return err
	}
	if err := writeColr(mw, cfg.Color.Selected); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // avc1
		return err
	}
	if _, err := mw.EndBox(); err != nil { // stsd
		return err
	}
	return nil
}

func buildAvcC(sps, pps []byte) []byte {
	if len(sps) < 4 {
		sps = []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	}
	if len(pps) == 0 {
		pps = []byte{0x68, 0xce, 0x38, 0x80}
	}
	var b bytes.Buffer
	b.WriteByte(1)
	b.WriteByte(sps[1])
	b.WriteByte(sps[2])
	b.WriteByte(sps[3])
	b.WriteByte(0xff)
	b.WriteByte(0xe1)
	binary.Write(&b, binary.BigEndian, uint16(len(sps)))
	b.Write(sps)
	b.WriteByte(1)
	binary.Write(&b, binary.BigEndian, uint16(len(pps)))
	b.Write(pps)
	return b.Bytes()
}

func writeColr(mw *gomp4.Writer, m ColorMetadata) error {
	p, tr, mat, err := NCLX(m.Standard)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("nclx")
	binary.Write(&b, binary.BigEndian, p)
	binary.Write(&b, binary.BigEndian, tr)
	binary.Write(&b, binary.BigEndian, mat)
	flag := byte(0)
	if m.FullRange {
		flag = 0x80
	}
	b.WriteByte(flag)
	return writeRawBox(mw, "colr", b.Bytes())
}

func writeStts(mw *gomp4.Writer, deltas []uint32) error {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(deltas)))
	for _, d := range deltas {
		binary.Write(&b, binary.BigEndian, uint32(1))
		binary.Write(&b, binary.BigEndian, d)
	}
	return writeFullBox(mw, "stts", 0, [3]byte{}, b.Bytes())
}

func writeStsz(mw *gomp4.Writer, sizes []uint32) error {
	var b bytes.Buffer
	common := sizes[0]
	allSame := true
	for _, s := range sizes[1:] {
		if s != common {
			allSame = false
			break
		}
	}
	if allSame {
		binary.Write(&b, binary.BigEndian, common)
		binary.Write(&b, binary.BigEndian, uint32(len(sizes)))
	} else {
		binary.Write(&b, binary.BigEndian, uint32(0))
		binary.Write(&b, binary.BigEndian, uint32(len(sizes)))
		for _, s := range sizes {
			binary.Write(&b, binary.BigEndian, s)
		}
	}
	return writeFullBox(mw, "stsz", 0, [3]byte{}, b.Bytes())
}
