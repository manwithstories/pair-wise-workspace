package ingest

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"

	"github.com/example/mediad/internal/model"
)

// MXFFixtureSpec describes one generated MXF KLV fixture.
type MXFFixtureSpec struct {
	Path              string
	Width, Height     uint32
	EditRate          [2]uint32
	VideoSamples      uint32
	KeyframeEvery     uint32
	ContainerRotation Rotation
	TrackRotation     Rotation
	ContainerCrop     *Crop
	TrackCrop         *Crop
	ContainerColor    *Color
	TrackColor        *Color
	AudioSampleRate   uint32
	AudioChannels     uint16
	AudioLayout       []string
	Corrupt           bool
}

// GenerateMXF writes a deterministic MXF-flavored KLV fixture.
func GenerateMXF(spec MXFFixtureSpec) (string, error) {
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o755); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.Write(mxfMagic)
	k := klw{&buf}

	// Preface: container-scope descriptors.
	pf := []byte{}
	pf = append(pf, encField(MXFTagEditRate, mxfPair32,
		putPair(spec.EditRate[0], spec.EditRate[1]))...)
	pf = append(pf, encField(MXFTagRotation, mxfU32,
		putU32(uint32(spec.ContainerRotation)))...)
	if spec.ContainerCrop != nil {
		pf = append(pf, encField(MXFTagCrop, mxfCrop32,
			putCrop(*spec.ContainerCrop))...)
	}
	if spec.ContainerColor != nil {
		pf = append(pf, encColorFields(spec.ContainerColor)...)
	}
	if spec.Corrupt {
		pf = append(pf, encField(MXFTagCorruptMarker, mxfU32, putU32(1))...)
	}
	k.item([]byte{0x06, 0x01, 0x01, 0x01}, pf)

	// Video essence track.
	vt := []byte{}
	vt = append(vt, encField(MXFTagTrackID, mxfU32, putU32(1))...)
	vt = append(vt, encField(MXFTagTrackName, mxfString,
		[]byte("picture-track"))...)
	vt = append(vt, encField(MXFTagPictureEssence, mxfU32, putU32(1))...)
	vt = append(vt, encField(MXFTagWidth, mxfU32, putU32(spec.Width))...)
	vt = append(vt, encField(MXFTagHeight, mxfU32, putU32(spec.Height))...)
	vt = append(vt, encField(MXFTagDuration, mxfU64,
		putU64(uint64(spec.VideoSamples)))...)
	vt = append(vt, encField(MXFTagRotation, mxfU32,
		putU32(uint32(spec.TrackRotation)))...)
	if spec.TrackCrop != nil {
		vt = append(vt, encField(MXFTagCrop, mxfCrop32,
			putCrop(*spec.TrackCrop))...)
	}
	if spec.TrackColor != nil {
		vt = append(vt, encColorFields(spec.TrackColor)...)
	}
	if spec.KeyframeEvery > 0 {
		var idx []byte
		for s := uint32(1); s <= spec.VideoSamples; s += spec.KeyframeEvery {
			idx = append(idx, putU32(s)...)
		}
		vt = append(vt, encField(MXFTagKeyframeIndex, mxfBytes, idx)...)
	}
	k.item([]byte{0x06, 0x01, 0x02, 0x01}, vt)

	// Sound essence track (optional).
	if spec.AudioSampleRate != 0 {
		stb := []byte{}
		stb = append(stb, encField(MXFTagTrackID, mxfU32, putU32(2))...)
		stb = append(stb, encField(MXFTagTrackName, mxfString,
			[]byte("sound-track"))...)
		stb = append(stb, encField(MXFTagSoundEssence, mxfU32, putU32(1))...)
		stb = append(stb, encField(MXFTagSampleRate, mxfU32,
			putU32(spec.AudioSampleRate))...)
		stb = append(stb, encField(MXFTagChannels, mxfU32,
			putU32(uint32(spec.AudioChannels)))...)
		layout := bytes.Join(spec.bytesLayout(), []byte{0})
		stb = append(stb, encField(MXFTagChannelLayout, mxfString, layout)...)
		k.item([]byte{0x06, 0x01, 0x02, 0x02}, stb)
	}
	if err := os.WriteFile(spec.Path, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	return spec.Path, nil
}

func (s MXFFixtureSpec) bytesLayout() [][]byte {
	out := make([][]byte, len(s.AudioLayout))
	for i, l := range s.AudioLayout {
		out[i] = []byte(l)
	}
	return out
}

// encField wraps one local tag with a one-byte type discriminator.
func encField(tag MXFTag, typ mxfValueType, payload []byte) []byte {
	out := localKey(tag)
	out = append(out, berLen(len(payload)+1)...)
	out = append(out, byte(typ))
	out = append(out, payload...)
	return out
}

func encColorFields(c *Color) []byte {
	out := encField(MXFTagColorPrimaries, mxfU32, putU32(uint32(c.Primaries)))
	out = append(out, encField(MXFTagColorTransfer, mxfU32,
		putU32(uint32(c.Transfer)))...)
	out = append(out, encField(MXFTagColorMatrix, mxfU32,
		putU32(uint32(c.Matrix)))...)
	if c.ICCProfile {
		out = append(out, encField(MXFTagColorICC, mxfBytes,
			[]byte("icc"))...)
	}
	return out
}

// ParseMXF decodes an MXF KLV fixture into a normalized Summary.
func ParseMXF(r io.Reader, path string) (*Summary, error) {
	magic := make([]byte, len(mxfMagic))
	if _, err := readFull(r, magic); err != nil {
		return nil, model.Wrap(model.KindCorrupt, "ingest", "mxf-parse",
			path, err)
	}
	if !bytes.Equal(magic, mxfMagic) {
		return nil, model.New(model.KindCorrupt, "ingest", "mxf-parse",
			path+": bad MXF magic")
	}
	file := &MXFFile{}
	for {
		key := make([]byte, 4)
		if _, err := readFull(r, key); err != nil {
			break // clean EOF
		}
		length, err := readBERLen(r)
		if err != nil {
			return nil, model.Wrap(model.KindCorrupt, "ingest",
				"mxf-parse", path, err)
		}
		payload := make([]byte, length)
		if _, err := readFull(r, payload); err != nil {
			return nil, model.Wrap(model.KindCorrupt, "ingest",
				"mxf-parse", path, err)
		}
		fields, err := decodeFields(payload)
		if err != nil {
			return nil, model.Wrap(model.KindCorrupt, "ingest",
				"mxf-parse", path, err)
		}
		switch {
		case key[2] == 0x01:
			file.HeaderFields = fields
		case key[2] == 0x02:
			file.Tracks = append(file.Tracks, MXFTrack{Fields: fields})
		}
	}
	return mxfToSummary(file, path)
}

func readFull(r io.Reader, b []byte) (int, error) { return io.ReadFull(r, b) }

func decodeFields(payload []byte) ([]mxfField, error) {
	var out []mxfField
	for len(payload) >= 5 {
		tag := MXFTag(binary.BigEndian.Uint16(payload[2:4]))
		// payload layout: 4-byte key, BER length, type byte, value
		br := bytes.NewReader(payload[4:])
		length, err := readBERLen(br)
		if err != nil {
			return nil, err
		}
		rest := payload[4+len(berLen(length)):]
		if len(rest) < length {
			return nil, io.ErrUnexpectedEOF
		}
		typ := mxfValueType(rest[0])
		value := rest[1:length]
		f, err := decodeField(tag, typ, value)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
		consumed := 4 + len(berLen(length)) + length
		payload = payload[consumed:]
	}
	return out, nil
}

func decodeField(tag MXFTag, typ mxfValueType, v []byte) (mxfField, error) {
	f := mxfField{Tag: tag, Type: typ, Bytes: v}
	switch typ {
	case mxfU32:
		if len(v) != 4 {
			return f, io.ErrUnexpectedEOF
		}
		f.U32 = binary.BigEndian.Uint32(v)
	case mxfU64:
		if len(v) != 8 {
			return f, io.ErrUnexpectedEOF
		}
		f.U64 = binary.BigEndian.Uint64(v)
	case mxfString:
		f.Str = string(v)
	case mxfPair32:
		if len(v) != 8 {
			return f, io.ErrUnexpectedEOF
		}
		f.Pair = [2]uint32{binary.BigEndian.Uint32(v[0:4]),
			binary.BigEndian.Uint32(v[4:8])}
	case mxfCrop32:
		if len(v) != 16 {
			return f, io.ErrUnexpectedEOF
		}
		f.Crop = Crop{
			X:      int64(binary.BigEndian.Uint32(v[0:4])),
			Y:      int64(binary.BigEndian.Uint32(v[4:8])),
			Width:  int64(binary.BigEndian.Uint32(v[8:12])),
			Height: int64(binary.BigEndian.Uint32(v[12:16])),
		}
	case mxfBytes:
		// kept verbatim
	default:
		return f, model.New(model.KindCorrupt, "ingest", "mxf-parse",
			"unknown local-tag value type")
	}
	return f, nil
}
