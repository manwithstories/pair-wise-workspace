// Package mp4 writes deterministic fragmented MP4/LPCM output using go-mp4's
// box writer. All timestamps in container metadata are fixed at zero so repeated
// runs produce byte-identical files.
package mp4

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/pcm"
)

// DefaultFragmentFrames is 100 ms at 48 kHz.
const DefaultFragmentFrames = 4800

// seekBuffer is an in-memory io.WriteSeeker used by go-mp4's box patching.
type seekBuffer struct {
	b   []byte
	pos int64
}

func (s *seekBuffer) Write(p []byte) (int, error) {
	end := s.pos + int64(len(p))
	if end > int64(len(s.b)) {
		grown := make([]byte, end)
		copy(grown, s.b)
		s.b = grown
	}
	copy(s.b[s.pos:end], p)
	s.pos = end
	return len(p), nil
}

func (s *seekBuffer) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = s.pos + offset
	case io.SeekEnd:
		next = int64(len(s.b)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("negative seek position")
	}
	if next > int64(cap(s.b)) {
		// Allow seeking a little beyond current length; the next Write grows.
		s.b = append(s.b, make([]byte, next-int64(len(s.b)))...)
	}
	s.pos = next
	return next, nil
}

// Render writes ftyp/moov plus one moof/mdat fragment per fragmentFrames.
// It returns the complete MP4 bytes and SHA-256.
func Render(audio *pcm.Audio, fragmentFrames int) ([]byte, string, error) {
	if audio == nil {
		return nil, "", fmt.Errorf("nil audio")
	}
	if audio.SampleRate != 48000 || audio.Channels != 2 {
		return nil, "", fmt.Errorf("MP4 render requires stereo 48 kHz, got %d ch %d Hz", audio.Channels, audio.SampleRate)
	}
	if fragmentFrames <= 0 {
		fragmentFrames = DefaultFragmentFrames
	}
	if audio.Frames%fragmentFrames != 0 {
		return nil, "", fmt.Errorf("frames %d not divisible by fragment duration %d", audio.Frames, fragmentFrames)
	}

	sb := &seekBuffer{}
	w := gomp4.NewWriter(sb)
	duration := uint32(audio.Frames)
	if err := writeFTYP(w); err != nil {
		return nil, "", fmt.Errorf("ftyp: %w", err)
	}
	if err := writeMOOV(w, duration); err != nil {
		return nil, "", fmt.Errorf("moov: %w", err)
	}

	pcmBytes := audio.ToS16BE()
	frameBytes := audio.Channels * 2
	fragments := audio.Frames / fragmentFrames
	for i := 0; i < fragments; i++ {
		startFrame := i * fragmentFrames
		startByte := startFrame * frameBytes
		payloadBytes := fragmentFrames * frameBytes
		payload := pcmBytes[startByte : startByte+payloadBytes]
		if err := writeFragment(w, uint32(i+1), uint32(startFrame), uint32(fragmentFrames), payload); err != nil {
			return nil, "", err
		}
	}

	sum := sha256.Sum256(sb.b)
	return sb.b, hex.EncodeToString(sum[:]), nil
}

// writeFragment writes one moof and mdat. The trun contains one sample whose
// duration is the whole fragment. With our fixed-width boxes the moof size is
// always 112 bytes, so the data offset is 112 + 8 byte mdat header = 120.
func writeFragment(w *gomp4.Writer, sequence, decodeTime, duration uint32, payload []byte) error {
	const moofSize = 112
	const dataOffset = moofSize + 8

	if err := startBox(w, "moof"); err != nil {
		return err
	}
	if err := startBox(w, "mfhd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	if err := writeBE(w, sequence); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // mfhd
		return err
	}

	if err := startBox(w, "traf"); err != nil {
		return err
	}
	if err := startBox(w, "tfhd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0x020000); err != nil { // default-base-is-moof
		return err
	}
	if err := writeBE(w, uint32(1)); err != nil { // track ID
		return err
	}
	if err := endBox(w); err != nil { // tfhd
		return err
	}

	if err := startBox(w, "tfdt"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	if err := writeBE(w, decodeTime); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // tfdt
		return err
	}

	if err := startBox(w, "trun"); err != nil {
		return err
	}
	// data-offset-present | sample-duration-present | sample-size-present
	if err := writeFullHeader(w, 0, 0x000301); err != nil {
		return err
	}
	if err := writeBE(w, uint32(1)); err != nil { // sample count
		return err
	}
	if err := writeBE(w, uint32(dataOffset)); err != nil {
		return err
	}
	if err := writeBE(w, duration); err != nil {
		return err
	}
	if err := writeBE(w, uint32(len(payload))); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // trun
		return err
	}
	if err := endBox(w); err != nil { // traf
		return err
	}
	if err := endBox(w); err != nil { // moof
		return err
	}

	if err := startBox(w, "mdat"); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return endBox(w)
}
