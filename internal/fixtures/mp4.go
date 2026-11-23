package fixtures

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Mp4Bundle is a generated MP4 input together with the identities the tests
// assert against.
type Mp4Bundle struct {
	Name          string
	Data          []byte
	VideoSizes    []int
	AudioSizes    []int
	VideoPayloads [][]byte
	AudioPayloads [][]byte
	VideoHash     [32]byte
	AudioHash     [32]byte
	// TruncatedAt is set for damaged bundles and names the cut offset.
	TruncatedAt int64
}

// BuildMP4 builds a two-track fast-start MP4 with coded samples copied into one
// mdat chunk per track. The sample entries carry avcC, colr and clap boxes so
// display metadata arbitration has something real to read.
func BuildMP4(o Options) (*Mp4Bundle, error) {
	o, err := o.validated()
	if err != nil {
		return nil, err
	}
	b := &Mp4Bundle{Name: "studio.mp4"}

	videoTimescale := uint32(o.FPSNum)
	audioTimescale := uint32(o.SampleRate)

	for i := 0; i < o.VideoFrames; i++ {
		size := sampleSize(o.Seed, o.VideoTrackID, i, 6400, 600)
		p := Payload(o.Seed, o.VideoTrackID, i, size)
		b.VideoSizes = append(b.VideoSizes, size)
		b.VideoPayloads = append(b.VideoPayloads, p)
	}
	for i := 0; i < o.AudioSamples; i++ {
		size := sampleSize(o.Seed, o.AudioTrackID, i, 980, 64)
		p := Payload(o.Seed, o.AudioTrackID, i, size)
		b.AudioSizes = append(b.AudioSizes, size)
		b.AudioPayloads = append(b.AudioPayloads, p)
	}
	if b.VideoHash, err = HashSamples(b.VideoPayloads); err != nil {
		return nil, err
	}
	if b.AudioHash, err = HashSamples(b.AudioPayloads); err != nil {
		return nil, err
	}

	ftyp := makeFtyp()
	// moov size does not depend on chunk offset values (stco entries are fixed
	// width), so building once with zero offsets gives a stable length.
	moovLen := len(makeMoov(o, videoTimescale, audioTimescale, 0, 0))
	mdatHeader := 8
	videoChunk := ftypLen + moovLen + mdatHeader
	audioChunk := videoChunk + totalBytes(b.VideoSizes)

	moov := makeMoov(o, videoTimescale, audioTimescale, uint32(videoChunk), uint32(audioChunk))
	mdat := box(nil, "mdat", appendPayloads(b.VideoPayloads, b.AudioPayloads)...)

	b.Data = append(append(append([]byte{}, ftyp...), moov...), mdat...)
	if len(b.Data) > maxFixtureBytes {
		return nil, fmt.Errorf("fixtures: generated MP4 exceeds %d bytes", maxFixtureBytes)
	}
	return b, nil
}

const ftypLen = 8 + 4 + 4 + 4*4

func makeMoov(o Options, videoTS, audioTS uint32, videoOff, audioOff uint32) []byte {
	mv := makeMvhd(o, videoTS, audioTS)
	vt := makeVideoTrak(o, videoTS, videoOff)
	at := makeAudioTrak(o, audioTS, audioOff)
	return box(nil, "moov", mv, vt, at)
}

func makeFtyp() []byte {
	p := append([]byte("isom"), be32(0)...)
	for _, brand := range []string{"isom", "iso2", "avc1", "mp41"} {
		p = append(p, brand...)
	}
	return box(nil, "ftyp", p)
}

func appendPayloads(sets ...[][]byte) [][]byte {
	var out [][]byte
	for _, set := range sets {
		out = append(out, set...)
	}
	return out
}

func totalBytes(sizes []int) int {
	n := 0
	for _, s := range sizes {
		n += s
	}
	return n
}

// variantUnsupportedColour flips the colour codes to values outside the known
// set while leaving everything else valid.
func variantUnsupportedColour(o Options) Options {
	o.UnknownColour = true
	o.ColourPrimaries, o.ColourTransfer, o.ColourMatrix = 0, 0, 0
	return o
}

// BuildMP4UnknownColour returns an MP4 whose colr box uses reserved codes.
func BuildMP4UnknownColour(o Options) (*Mp4Bundle, error) {
	o = variantUnsupportedColour(o)
	o.CropPixels = 0
	b, err := BuildMP4(o)
	if err != nil {
		return nil, err
	}
	b.Name = "unknown-colour.mp4"
	return b, nil
}

// BuildMP4CropOutOfBounds returns an MP4 whose clap box removes more pixels than
// the frame contains. Readers must reject it rather than emit a negative crop.
func BuildMP4CropOutOfBounds(o Options) (*Mp4Bundle, error) {
	// Clean aperture wider than the coded frame means a negative crop.
	o.CropPixels = 0
	o.ClapCleanW = o.Width + 64
	o.ClapCleanH = o.Height
	b, err := BuildMP4(o)
	if err != nil {
		return nil, err
	}
	b.Name = "crop-out-of-bounds.mp4"
	return b, nil
}

// BuildMP4Truncated cuts the generated file in the middle of the final stbl
// child so the parser sees a box header that claims more bytes than remain.
func BuildMP4Truncated(o Options) (*Mp4Bundle, error) {
	b, err := BuildMP4(o)
	if err != nil {
		return nil, err
	}
	if len(b.Data) < 200 {
		return nil, errors.New("fixtures: base MP4 too small to truncate")
	}
	// Find the moov box and cut inside it, after at least one trak so the
	// parser has to attribute the failure to a track.
	moovOff := indexOfBox(b.Data, "moov")
	if moovOff < 0 {
		return nil, errors.New("fixtures: moov not found")
	}
	cut := moovOff + (len(b.Data)-moovOff)/2
	// Ensure the cut lands strictly inside the moov payload.
	if cut >= len(b.Data)-8 {
		cut = len(b.Data) - 8
	}
	b.Data = b.Data[:cut]
	b.Name = "truncated.mp4"
	b.TruncatedAt = int64(cut)
	return b, nil
}

// indexOfBox finds the byte offset of a top-level box by scanning headers.
func indexOfBox(data []byte, typ string) int {
	off := 0
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off:]))
		if size < 8 || off+size > len(data) {
			return -1
		}
		if string(data[off+4:off+8]) == typ {
			return off
		}
		off += size
	}
	return -1
}
