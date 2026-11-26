package wavscan

import (
	"encoding/binary"
	"io"

	"archivectl/internal/core"
)

const (
	IDRIFF = "RIFF"
	IDWAVE = "WAVE"
	IDFmt  = "fmt "
	IDData = "data"
	IDCue  = "cue "
)

// Format is the validated PCM WAV format header.
type Format struct {
	AudioFormat   uint16 `json:"audio_format"`
	Channels      int    `json:"channels"`
	SampleRate    int    `json:"sample_rate"`
	ByteRate      int64  `json:"byte_rate"`
	BlockAlign    int    `json:"block_align"`
	BitsPerSample int    `json:"bits_per_sample"`
}

func (f Format) BytesPerSample() int { return f.BitsPerSample / 8 }
func (f Format) FrameBytes() int     { return f.Channels * f.BytesPerSample() }

// Validate enforces the archive's supported PCM envelope.
func (f Format) Validate(op string) error {
	if f.AudioFormat != 1 {
		return core.Newf(core.KindUnsupported, op,
			"audio format %d is not raw PCM (1)", f.AudioFormat)
	}
	if f.Channels < 1 || f.Channels > 8 {
		return core.Newf(core.KindUnsupported, op,
			"channels %d outside supported range 1..8", f.Channels)
	}
	switch f.BitsPerSample {
	case 8, 16, 24, 32:
	default:
		return core.Newf(core.KindUnsupported, op,
			"bits per sample %d is not one of 8/16/24/32", f.BitsPerSample)
	}
	if f.SampleRate <= 0 {
		return core.New(core.KindMalformedFMT, op, "sample rate must be positive")
	}
	if f.BlockAlign != f.FrameBytes() {
		return core.Newf(core.KindAlignment, op,
			"block align %d does not match channels*bits/8=%d", f.BlockAlign, f.FrameBytes())
	}
	if f.ByteRate != int64(f.SampleRate)*int64(f.FrameBytes()) {
		return core.Newf(core.KindAlignment, op,
			"byte rate %d does not match sample_rate*frame_bytes=%d",
			f.ByteRate, int64(f.SampleRate)*int64(f.FrameBytes()))
	}
	return nil
}

// ChunkInfo describes one RIFF chunk without retaining its contents.
type ChunkInfo struct {
	Index          int    `json:"index"`
	ID             string `json:"id"`
	HeaderOffset   int64  `json:"header_offset"`
	DataOffset     int64  `json:"data_offset"`
	Size           int64  `json:"size"`
	OddPadding     bool   `json:"odd_padding"`
	PaddingPresent bool   `json:"padding_present"`
	Truncated      bool   `json:"truncated"`
	Unknown        bool   `json:"unknown"`
}

// CuePoint is the decoded cue point layout (24 bytes per point).
type CuePoint struct {
	ID           uint32 `json:"id"`
	Position     uint32 `json:"position"`
	DataChunkID  string `json:"data_chunk_id"`
	ChunkStart   uint32 `json:"chunk_start"`
	BlockStart   uint32 `json:"block_start"`
	SampleOffset uint32 `json:"sample_offset"`
	Frame        int64  `json:"frame"`
}

func fourCC(b []byte) string {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return string(b)
}

func readFullAt(r io.ReaderAt, off int64, p []byte, op string) error {
	n, err := r.ReadAt(p, off)
	if err != nil && err != io.EOF {
		return core.Wrap(core.KindIO, op, "", err)
	}
	if n != len(p) {
		return core.Newf(core.KindChunkLength, op,
			"short read at %d: got %d bytes, want %d", off, n, len(p))
	}
	return nil
}

func decodeFmt(body []byte) (Format, error) {
	const op = "wavscan.decodeFmt"
	if len(body) < 16 {
		return Format{}, core.Newf(core.KindMalformedFMT, op,
			"fmt chunk is %d bytes; need at least 16", len(body))
	}
	tag := binary.LittleEndian.Uint16(body[0:2])
	ch := binary.LittleEndian.Uint16(body[2:4])
	rate := binary.LittleEndian.Uint32(body[4:8])
	byteRate := binary.LittleEndian.Uint32(body[8:12])
	blockAlign := binary.LittleEndian.Uint16(body[12:14])
	bits := binary.LittleEndian.Uint16(body[14:16])
	f := Format{
		AudioFormat:   tag,
		Channels:      int(ch),
		SampleRate:    int(rate),
		ByteRate:      int64(byteRate),
		BlockAlign:    int(blockAlign),
		BitsPerSample: int(bits),
	}
	if err := f.Validate(op); err != nil {
		return Format{}, err
	}
	return f, nil
}

func decodeCue(body []byte, declaredSize int64) ([]CuePoint, error) {
	const op = "wavscan.decodeCue"
	if declaredSize < 4 || len(body) < 4 {
		return nil, core.Newf(core.KindChunkLength, op, "cue chunk too short: %d bytes", declaredSize)
	}
	count := binary.LittleEndian.Uint32(body[0:4])
	want := 4 + int64(count)*24
	if declaredSize != want {
		return nil, core.Newf(core.KindChunkLength, op,
			"cue chunk length %d does not match count %d (want %d)", declaredSize, count, want)
	}
	if int64(len(body)) < want {
		return nil, core.Newf(core.KindChunkLength, op, "cue chunk truncated")
	}
	points := make([]CuePoint, 0, count)
	for i := uint32(0); i < count; i++ {
		off := 4 + int(i)*24
		var raw [24]byte
		copy(raw[:], body[off:off+24])
		id := binary.LittleEndian.Uint32(raw[0:4])
		dataID := fourCC(raw[8:12])
		sampleOffset := binary.LittleEndian.Uint32(raw[20:24])
		points = append(points, CuePoint{
			ID:           id,
			Position:     binary.LittleEndian.Uint32(raw[4:8]),
			DataChunkID:  dataID,
			ChunkStart:   binary.LittleEndian.Uint32(raw[12:16]),
			BlockStart:   binary.LittleEndian.Uint32(raw[16:20]),
			SampleOffset: sampleOffset,
			Frame:        int64(sampleOffset),
		})
	}
	return points, nil
}
