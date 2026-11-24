// Package pack writes deterministic LPCM-in-MP4 artifacts and resumable raw
// PCM staging data.
package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"

	"github.com/clearliu/mediq/internal/core"
)

// Options describes the output LPCM format.
type Options struct {
	SampleRate int
	Channels   int
}

// Product describes a written MP4 artifact.
type Product struct {
	Path     string
	Bytes    int64
	Frames   int
	PCMBytes int
	SHA256   string
}

// WriteLPCM writes a deterministic MP4 containing signed 16-bit little-endian
// interleaved PCM. mdat precedes moov so the sole stco offset is known without
// patching. The file is replaced atomically.
func WriteLPCM(finalPath string, pcm []byte, opts Options) (*Product, error) {
	if opts.SampleRate <= 0 {
		opts.SampleRate = core.OutputRateHz
	}
	if opts.Channels <= 0 {
		opts.Channels = core.OutputChannels
	}
	blockAlign := opts.Channels * 2
	if len(pcm)%blockAlign != 0 {
		return nil, core.New(core.KindConfig, "write-mp4",
			fmt.Errorf("pcm bytes %d not divisible by block alignment %d", len(pcm), blockAlign))
	}
	frames := len(pcm) / blockAlign
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return nil, core.New(core.KindIO, "create-product-dir", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(finalPath), ".mediq-*.mp4.tmp")
	if err != nil {
		return nil, core.New(core.KindIO, "create-mp4-temp", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	b := &builder{w: mp4.NewWriter(tmp)}

	// ftyp: isom/mp42 file.
	b.begin("ftyp")
	b.raw([]byte("isom"))
	b.u32(512)
	b.raw([]byte("isom"))
	b.raw([]byte("mp42"))
	b.end()

	// mdat first. ftyp is 24 bytes and mdat's header is 8, so data starts 32.
	const chunkOffset = uint32(32)
	b.begin("mdat")
	b.raw(pcm)
	b.end()

	b.begin("moov")
	writeMVHD(b, uint32(opts.SampleRate), uint32(frames))
	b.begin("trak")
	writeTKHD(b, uint32(frames))
	b.begin("mdia")
	writeMDHD(b, uint32(opts.SampleRate), uint32(frames))
	writeHDLR(b)
	b.begin("minf")
	writeSMHD(b)
	writeDinf(b)
	b.begin("stbl")
	writeStsd(b, uint16(opts.Channels), uint32(opts.SampleRate))
	writeStts(b, uint32(frames))
	writeStsc(b, uint32(frames))
	writeStsz(b, uint32(blockAlign), uint32(frames))
	writeStco(b, chunkOffset)
	b.end() // stbl
	b.end() // minf
	b.end() // mdia
	b.end() // trak
	b.end() // moov

	if b.err != nil {
		_ = tmp.Close()
		return nil, core.New(core.KindIO, "encode-mp4", b.err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, core.New(core.KindIO, "sync-mp4", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, core.New(core.KindIO, "close-mp4", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return nil, core.New(core.KindIO, "chmod-mp4", err)
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return nil, core.New(core.KindIO, "rename-mp4", err)
	}
	cleanup = false

	sum, n, err := hashFile(finalPath)
	if err != nil {
		return nil, err
	}
	return &Product{Path: finalPath, Bytes: n, Frames: frames, PCMBytes: len(pcm), SHA256: sum}, nil
}

func writeMVHD(b *builder, timescale, duration uint32) {
	b.full("mvhd", 0)
	b.u32(0) // creation
	b.u32(0) // modification
	b.u32(timescale)
	b.u32(duration)
	b.u32(0x00010000) // rate 1.0
	b.u16(0x0100)     // volume 1.0
	b.u16(0)
	b.u32(0)
	b.u32(0)
	writeMatrix(b)
	for i := 0; i < 6; i++ {
		b.u32(0)
	}
	b.u32(2) // next track ID
	b.end()
}

func writeTKHD(b *builder, duration uint32) {
	b.full("tkhd", 0x000003) // enabled + in-movie
	b.u32(0)
	b.u32(0)
	b.u32(1) // track ID
	b.u32(0)
	b.u32(duration)
	b.u32(0)
	b.u32(0)
	b.u16(0) // layer
	b.u16(0) // alternate group
	b.u16(0x0100)
	b.u16(0)
	writeMatrix(b)
	b.u32(0) // width
	b.u32(0) // height
	b.end()
}

func writeMDHD(b *builder, timescale, duration uint32) {
	b.full("mdhd", 0)
	b.u32(0)
	b.u32(0)
	b.u32(timescale)
	b.u32(duration)
	b.u16(0x55c4) // und
	b.u16(0)
	b.end()
}

func writeHDLR(b *builder) {
	b.full("hdlr", 0)
	b.u32(0)
	b.raw([]byte("soun"))
	b.u32(0)
	b.u32(0)
	b.u32(0)
	b.raw([]byte{0}) // empty handler name
	b.end()
}

func writeSMHD(b *builder) {
	b.full("smhd", 0)
	b.u16(0) // balance
	b.u16(0)
	b.end()
}

func writeDinf(b *builder) {
	b.begin("dinf")
	b.full("dref", 0)
	b.u32(1)
	b.full("url ", 1) // self-contained flag
	b.end()
	b.end()
	b.end() // dref full-box helper began the box; full does not end it.
}

func writeStsd(b *builder, channels uint16, rate uint32) {
	b.full("stsd", 0)
	b.u32(1)
	b.begin("ipcm")
	b.raw(make([]byte, 6)) // reserved
	b.u16(1)               // data reference index
	b.u16(0)               // entry version
	b.u16(0)
	b.u16(0)
	b.u16(0) // three reserved uint16
	b.u16(channels)
	b.u16(16) // sample size
	b.u16(0)  // predefined
	b.u16(0)
	b.u32(rate << 16) // 16.16 fixed point

	b.full("pcmC", 0)
	b.raw([]byte{0x01, 16}) // little-endian, 16-bit
	b.end()

	b.end() // ipcm
	b.end() // stsd
}

func writeStts(b *builder, frames uint32) {
	b.full("stts", 0)
	b.u32(1)
	b.u32(frames)
	b.u32(1)
	b.end()
}

func writeStsc(b *builder, frames uint32) {
	b.full("stsc", 0)
	b.u32(1)
	b.u32(1)
	b.u32(frames)
	b.u32(1)
	b.end()
}

func writeStsz(b *builder, sampleSize, frames uint32) {
	b.full("stsz", 0)
	b.u32(sampleSize)
	b.u32(frames)
	b.end()
}

func writeStco(b *builder, offset uint32) {
	b.full("stco", 0)
	b.u32(1)
	b.u32(offset)
	b.end()
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, core.New(core.KindIO, "open-product-hash", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, core.New(core.KindIO, "hash-product", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
