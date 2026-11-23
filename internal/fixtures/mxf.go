package fixtures

import (
	"encoding/binary"
	"fmt"
)

// The MXF fixture is an operational-pattern-1-style stream using SMPTE 377M
// KLV framing: every pack or metadata set is a 16-byte key followed by a BER
// length and a value. Structural metadata sets hold two-byte local tags whose
// meaning is declared once in the primer pack, exactly as a real file does.
//
// The essence mapping is carried by a profile set (ulEssenceMap) rather than a
// full index segment: it names, per track, the ordinal range of essence KLV
// packets in this file. That is enough for a lossless sample import while
// keeping the fixture compact.

var (
	ulHeaderPartition = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x00}
	ulBodyPartition   = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x01}
	ulFooterPartition = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x05, 0x00}

	ulPrimer          = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	ulIdentification  = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x30, 0x00}
	ulMaterialPackage = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x36, 0x00}
	ulSourcePackage   = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x37, 0x00}
	ulTrack           = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x03, 0x01, 0x00}
	ulSequence        = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x0f, 0x01, 0x00}
	ulCDCI            = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x28, 0x01, 0x00}
	ulWave            = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x48, 0x01, 0x00}
	ulEssence         = []byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01, 0x16, 0x01, 0x00, 0x00}

	// ulEssenceMap is a mediasync profile set: it is not an SMPTE-defined UL,
	// so its 0x0e.2b.34.02.7f prefix keeps it in the private range.
	ulEssenceMap = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x7f, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x7f, 0x01, 0x00}
)

// Local tags used by the generated sets. The primer pack maps each to the UL
// of the property it stands for.
const (
	tagInstanceUID       = uint16(0x3c0a)
	tagGenerationUID     = uint16(0x0102)
	tagPackageUID        = uint16(0x4401)
	tagTrackCount        = uint16(0x4402)
	tagTrackSets         = uint16(0x4403)
	tagTrackID           = uint16(0x4801)
	tagTrackNumber       = uint16(0x4802)
	tagSequenceSet       = uint16(0x4803)
	tagDuration          = uint16(0x0202)
	tagEditRate          = uint16(0x4b01)
	tagOrigin            = uint16(0x0203)
	tagEventCount        = uint16(0x0501)
	tagEventEditRate     = uint16(0x0503)
	tagEssenceDescSets   = uint16(0x4804)
	tagFrameLayout       = uint16(0x3202)
	tagStoredWidth       = uint16(0x3203)
	tagStoredHeight      = uint16(0x3204)
	tagDisplayWidth      = uint16(0x3208)
	tagDisplayHeight     = uint16(0x3209)
	tagComponentDepth    = uint16(0x3301)
	tagHorizSubsampling  = uint16(0x3308)
	tagVertSubsampling   = uint16(0x3309)
	tagAudioSamplingRate = uint16(0x3d03)
	tagChannelCount      = uint16(0x3d07)
	tagQuantizationBits  = uint16(0x3d01)
	tagMapEntries        = uint16(0x7f01)
	tagMapTrackID        = uint16(0x7f02)
	tagMapKind           = uint16(0x7f03)
	tagMapFirstPacket    = uint16(0x7f04)
	tagMapPacketCount    = uint16(0x7f05)
	tagMapEditRate       = uint16(0x7f06)
)

// BERLength exposes the BER length encoder for MXF reader tests.
func BERLength(n int) []byte { return berLength(n) }

// klv frames a 16-byte key, BER length and value.
func klv(key, value []byte) []byte {
	out := make([]byte, 0, 16+5+len(value))
	out = append(out, key...)
	out = append(out, berLength(len(value))...)
	return append(out, value...)
}

// berLength encodes a KLV length in BER. Values under 128 use the short form;
// longer values use the minimum number of long-form bytes.
func berLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var tmp []byte
	for n > 0 {
		tmp = append([]byte{byte(n & 0xff)}, tmp...)
		n >>= 8
	}
	return append([]byte{0x80 | byte(len(tmp))}, tmp...)
}

// prop is a local set property: two-byte tag, BER length, value.
func prop(tag uint16, value []byte) []byte {
	out := []byte{byte(tag >> 8), byte(tag)}
	out = append(out, berLength(len(value))...)
	return append(out, value...)
}

// set frames a metadata set whose value is a run of properties.
func set(ul []byte, props ...[]byte) []byte {
	var v []byte
	for _, p := range props {
		v = append(v, p...)
	}
	return klv(ul, v)
}

func uint32Value(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func uint64Value(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// rationalValue encodes a 64-bit numerator and 32-bit denominator.
func rationalValue(num int64, den int32) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[0:8], uint64(num))
	binary.BigEndian.PutUint32(b[8:12], uint32(den))
	return b
}

// uuidValue builds a deterministic 16-byte instance or package ID.
func uuidValue(seed int64, kind byte) []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(seed>>(uint(i%7)*8)) ^ byte(i*17) ^ kind
	}
	return b
}

// BuildMxfOptions controls the generated stream.
type BuildMxfOptions struct {
	Seed                       int64
	VideoSamples, AudioSamples int
	VideoEditRateNum           int64
	VideoEditRateDen           int32
	AudioEditRate              int32
	VideoTrackID, AudioTrackID uint32
	StoredWidth, StoredHeight  int
	FrameLayout                byte
}

// DefaultMxfOptions returns a two-track MXF fixture.
func DefaultMxfOptions() BuildMxfOptions {
	return BuildMxfOptions{
		Seed: 41, VideoSamples: 25, AudioSamples: 40,
		VideoEditRateNum: 30000, VideoEditRateDen: 1001,
		AudioEditRate: 48000,
		VideoTrackID:  1, AudioTrackID: 2,
		StoredWidth: 1920, StoredHeight: 1080,
	}
}

func (o BuildMxfOptions) validated() (BuildMxfOptions, error) {
	if o.VideoSamples < 0 || o.AudioSamples < 0 {
		return o, fmt.Errorf("fixtures: negative MXF sample counts")
	}
	if o.VideoEditRateNum <= 0 || o.VideoEditRateDen <= 0 {
		return o, fmt.Errorf("fixtures: invalid MXF video edit rate")
	}
	if o.AudioEditRate <= 0 {
		return o, fmt.Errorf("fixtures: invalid MXF audio edit rate")
	}
	if o.StoredWidth <= 0 || o.StoredHeight <= 0 {
		return o, fmt.Errorf("fixtures: invalid MXF dimensions")
	}
	if o.VideoTrackID == 0 || o.AudioTrackID == 0 || o.VideoTrackID == o.AudioTrackID {
		return o, fmt.Errorf("fixtures: MXF track IDs must be distinct and non-zero")
	}
	return o, nil
}

// MxfTrackInfo describes one essence track the generated file declares.
type MxfTrackInfo struct {
	TrackID     uint32
	Kind        string
	EditRateNum int64
	EditRateDen int32
	Sizes       []int
	Payloads    [][]byte
	Hash        [32]byte
	FirstPacket int
}

// MxfBundle is a generated MXF input plus sample identities.
type MxfBundle struct {
	Name        string
	Data        []byte
	Stats       []MxfTrackInfo
	TruncatedAt int64
}
