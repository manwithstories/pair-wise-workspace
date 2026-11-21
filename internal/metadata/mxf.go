package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// The MXF support here is deliberately a minimal, project-defined KLV subset:
// a fixed 16-byte header, a 0x06/0x0E primer placeholder, and local-set KLVs
// carrying the metadata fields this pipeline needs. It is not a general MXF
// reader and never touches essence, so no external media or online service is
// involved.
const (
	mxfMagic   = "MEDIADMXF"
	mxfVersion = 1
	// 9-byte magic + version + track count + 6 reserved bytes.
	mxfHeaderSize = 17
	mxfPrimerLen  = 8
	// Local-set tags (project-defined, one byte each since keys are short).
	tagTrackID     = 0x01
	tagKind        = 0x02
	tagEditRateNum = 0x03
	tagEditRateDen = 0x04
	tagDuration    = 0x05
	tagWidth       = 0x06
	tagHeight      = 0x07
	tagRotation    = 0x08
	tagCropX       = 0x09
	tagCropY       = 0x0A
	tagCropW       = 0x0B
	tagCropH       = 0x0C
	tagPrimaries   = 0x0D
	tagTransfer    = 0x0E
	tagMatrix      = 0x0F
	tagFullRange   = 0x10
	tagSampleRate  = 0x11
	tagChannels    = 0x12
	tagEnd         = 0x00
)

// MXFTrackSpec is one track in a generated MXF sample.
type MXFTrackSpec struct {
	Kind        Kind
	ID          uint32
	EditRateNum int64
	EditRateDen int64
	Duration    int64
	Width       int
	Height      int
	Rotation    int
	Crop        *Crop
	// Primaries/Transfer/Matrix of zero mean "not described" and are omitted
	// from the KLV stream.
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
	SampleRate, Channels        int
}

// MXFSpec is a generated MXF metadata sample.
type MXFSpec struct {
	Tracks []MXFTrackSpec
	// CorruptEditRate writes a zero denominator.
	CorruptEditRate bool
	// CorruptCrop writes an aperture larger than the frame.
	CorruptCrop bool
}

type klv struct {
	tag byte
	val []byte
}

func putUint(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func trackKLV(s MXFTrackSpec, idx int) []klv {
	mk := func(tag byte, v uint64) klv { return klv{tag: tag, val: putUint(v)} }
	kind := byte(0)
	if s.Kind == KindAudio {
		kind = 1
	}
	out := []klv{
		mk(tagTrackID, uint64(s.ID)),
		{tag: tagKind, val: []byte{kind}},
		mk(tagEditRateNum, uint64(s.EditRateNum)),
		mk(tagEditRateDen, uint64(s.EditRateDen)),
		mk(tagDuration, uint64(s.Duration)),
	}
	if s.Kind == KindVideo {
		out = append(out, mk(tagWidth, uint64(s.Width)), mk(tagHeight, uint64(s.Height)),
			mk(tagRotation, uint64(s.Rotation)))
		if s.Crop != nil {
			out = append(out, mk(tagCropX, uint64(s.Crop.X)), mk(tagCropY, uint64(s.Crop.Y)),
				mk(tagCropW, uint64(s.Crop.W)), mk(tagCropH, uint64(s.Crop.H)))
		}
		if s.Primaries != 0 {
			out = append(out, mk(tagPrimaries, uint64(s.Primaries)), mk(tagTransfer, uint64(s.Transfer)),
				mk(tagMatrix, uint64(s.Matrix)))
			full := uint64(0)
			if s.FullRange {
				full = 1
			}
			out = append(out, mk(tagFullRange, full))
		}
	} else {
		out = append(out, mk(tagSampleRate, uint64(s.SampleRate)), mk(tagChannels, uint64(s.Channels)))
	}
	_ = idx
	return out
}

// WriteMXF writes a minimal MXF metadata sample to path.
func WriteMXF(path string, spec MXFSpec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var body bytes.Buffer
	body.WriteString(mxfMagic)
	body.WriteByte(mxfVersion)
	body.WriteByte(byte(len(spec.Tracks)))
	body.Write([]byte{0, 0, 0, 0, 0, 0}) // reserved
	for _, k := range []klv{
		{tag: 0x06, val: []byte{0x0e, 0x00, 0x00, 0x00, 0x00}},
		{tag: 0x00, val: make([]byte, mxfPrimerLen)},
	} {
		body.WriteByte(k.tag)
		body.WriteByte(byte(len(k.val)))
		body.Write(k.val)
	}
	for i, t := range spec.Tracks {
		ns := trackKLV(t, i)
		if spec.CorruptEditRate && i == 0 {
			ns[3].val = putUint(0)
		}
		if spec.CorruptCrop && i == 0 {
			ns = append(ns, klv{tagCropX, putUint(0)}, klv{tagCropY, putUint(0)},
				klv{tagCropW, putUint(uint64(t.Width + 100))}, klv{tagCropH, putUint(uint64(t.Height))})
		}
		for _, k := range ns {
			body.WriteByte(k.tag)
			body.WriteByte(byte(len(k.val)))
			body.Write(k.val)
		}
		body.WriteByte(tagEnd)
	}
	return os.WriteFile(path, body.Bytes(), 0o644)
}

// ParseMXF parses the project-defined MXF metadata sample.
func ParseMXF(path string) (*Asset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.Wrap(model.CategoryMetadata, "read-mxf", err)
	}
	return ParseMXFBytes(path, data)
}

// ParseMXFBytes parses an in-memory MXF metadata sample.
func ParseMXFBytes(path string, data []byte) (*Asset, error) {
	if len(data) < mxfHeaderSize || string(data[:len(mxfMagic)]) != mxfMagic {
		return nil, model.New(model.CategoryMetadata, "parse-mxf", "bad magic or short header")
	}
	if data[len(mxfMagic)] != mxfVersion {
		return nil, model.New(model.CategoryMetadata, "parse-mxf", fmt.Sprintf("unsupported version %d", data[len(mxfMagic)]))
	}
	count := int(data[len(mxfMagic)+1])
	pos := mxfHeaderSize
	// Skip the two leading sets (primer placeholder and index placeholder),
	// each using its declared length.
	for skip := 0; skip < 2; skip++ {
		if pos+2 > len(data) {
			return nil, model.New(model.CategoryMetadata, "parse-mxf", "truncated KLV")
		}
		l := int(data[pos+1])
		if pos+2+l > len(data) {
			return nil, model.New(model.CategoryMetadata, "parse-mxf", "KLV length exceeds sample")
		}
		pos += 2 + l
	}
	if count == 0 {
		return &Asset{Path: path, Container: "mxf"}, nil
	}

	asset := &Asset{Path: path, Container: "mxf"}
	videoID, audioID := 0, 0
	for i := 0; i < count; i++ {
		fields := map[byte]uint64{}
		for {
			if pos >= len(data) {
				return nil, model.New(model.CategoryMetadata, "parse-mxf", "truncated track set")
			}
			tag := data[pos]
			if tag == tagEnd {
				pos++
				break
			}
			if pos+2 > len(data) {
				return nil, model.New(model.CategoryMetadata, "parse-mxf", "truncated KLV header")
			}
			l := int(data[pos+1])
			if pos+2+l > len(data) || l > 8 {
				return nil, model.New(model.CategoryMetadata, "parse-mxf", "invalid KLV length")
			}
			field := data[pos+2 : pos+2+l]
			pos += 2 + l
			var v uint64
			for _, b := range field {
				v = v<<8 | uint64(b)
			}
			fields[tag] = v
		}
		kind := KindVideo
		id := uint32(videoID)
		if fields[tagKind] == 1 {
			kind = KindAudio
			id = uint32(audioID)
		}
		key := model.TrackKey(string(kind), id)
		num, den := int64(fields[tagEditRateNum]), int64(fields[tagEditRateDen])
		tb, terr := clock.New(num, den)
		if terr != nil {
			err := model.Wrap(model.CategoryMetadata, "edit-rate", terr).WithTrack(key).
				WithBlockingDep("metadata:" + key)
			asset.Failures = append(asset.Failures, TrackFailure{Key: key, Err: err})
			continue
		}
		t := Track{
			Key: key, Kind: kind, TrackID: uint32(fields[tagTrackID]),
			Timebase: tb, Duration: int64(fields[tagDuration]),
			Codec: "mxf", Container: "mxf", Color: DefaultColor,
		}
		if kind == KindVideo {
			t.CodedWidth, t.CodedHeight = int(fields[tagWidth]), int(fields[tagHeight])
			t.DisplayWidth, t.DisplayHeight = t.CodedWidth, t.CodedHeight
			t.Rotation, t.RotationRaw = NormalizeRotation(float64(fields[tagRotation]))
			if _, ok := fields[tagCropW]; ok {
				t.Crop = &Crop{X: int(fields[tagCropX]), Y: int(fields[tagCropY]),
					W: int(fields[tagCropW]), H: int(fields[tagCropH])}
			}
			if _, ok := fields[tagPrimaries]; ok {
				t.Color = NewColor(uint16(fields[tagPrimaries]), uint16(fields[tagTransfer]),
					uint16(fields[tagMatrix]), fields[tagFullRange] == 1, SourceMXF)
			}
			if cerr := ValidateCrop(t); cerr != nil {
				asset.Failures = append(asset.Failures, TrackFailure{Key: key, Err: cerr.WithBlockingDep("metadata:" + key)})
				t.Crop = nil
				t.Notes = append(t.Notes, "clean aperture rejected: "+cerr.Detail)
			}
			if cerr := validateColor(t); cerr != nil {
				asset.Failures = append(asset.Failures, TrackFailure{Key: key, Err: cerr.WithBlockingDep("metadata:" + key)})
			}
			videoID++
		} else {
			t.SampleRate, t.Channels = int(fields[tagSampleRate]), int(fields[tagChannels])
			audioID++
		}
		asset.Tracks = append(asset.Tracks, t)
	}
	// Report the first categorized failure while still returning the tracks
	// that survived, so the CLI can exit non-zero and callers can keep the
	// healthy branches running.
	if len(asset.Failures) > 0 {
		return asset, asset.Failures[0].Err
	}
	return asset, nil
}

// demoMXFSpec mirrors the MP4 fixture in MXF form so cross-container
// normalization can be compared.
func demoMXFSpec(corrupt bool) MXFSpec {
	spec := MXFSpec{Tracks: []MXFTrackSpec{
		{Kind: KindVideo, ID: 1, EditRateNum: 1, EditRateDen: 30000, Duration: 60000,
			Width: 1920, Height: 1080, Rotation: 90,
			Primaries: PrimariesBT2020, Transfer: TransferPQ, Matrix: MatrixBT2020, FullRange: true},
		{Kind: KindVideo, ID: 2, EditRateNum: 1, EditRateDen: 30000, Duration: 60000,
			Width: 1280, Height: 720, Crop: &Crop{X: 0, Y: 0, W: 1024, H: 576},
			Primaries: PrimariesBT709, Transfer: TransferBT709, Matrix: MatrixBT709},
		{Kind: KindAudio, ID: 3, EditRateNum: 1, EditRateDen: 48000, Duration: 96000,
			SampleRate: 48000, Channels: 2},
	}}
	if corrupt {
		spec.CorruptEditRate = true
		spec.CorruptCrop = true
	}
	return spec
}

// WriteDemoMXF writes the standard MXF fixture to dir/sample.mxf.
func WriteDemoMXF(dir string, corrupt bool) (string, error) {
	path := filepath.Join(dir, "sample.mxf")
	if err := WriteMXF(path, demoMXFSpec(corrupt)); err != nil {
		return "", err
	}
	return path, nil
}
