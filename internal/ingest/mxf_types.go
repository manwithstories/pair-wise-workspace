package ingest

// MXFTag is a minimal SMPTE ST 377 KLV local-tag identifier used by the
// self-generated MXF fixtures. Only the subset needed for track
// summaries, rotation/crop/color descriptors and the audio layout is
// modeled.
type MXFTag uint16

const (
	MXFTagPreface        MXFTag = 0x0101
	MXFTagEssenceTrack   MXFTag = 0x0201
	MXFTagTrackID        MXFTag = 0x0202
	MXFTagTrackName      MXFTag = 0x0203
	MXFTagEditRate       MXFTag = 0x0204
	MXFTagDuration       MXFTag = 0x0205
	MXFTagSampleRate     MXFTag = 0x0206
	MXFTagChannels       MXFTag = 0x0207
	MXFTagChannelLayout  MXFTag = 0x0208
	MXFTagWidth          MXFTag = 0x0209
	MXFTagHeight         MXFTag = 0x020a
	MXFTagCodec          MXFTag = 0x020b
	MXFTagPictureEssence MXFTag = 0x020c
	MXFTagSoundEssence   MXFTag = 0x020d
	MXFTagRotation       MXFTag = 0x0301
	MXFTagCrop           MXFTag = 0x0302
	MXFTagColorPrimaries MXFTag = 0x0303
	MXFTagColorTransfer  MXFTag = 0x0304
	MXFTagColorMatrix    MXFTag = 0x0305
	MXFTagColorICC       MXFTag = 0x0306
	MXFTagKeyframeIndex  MXFTag = 0x0401
	MXFTagCorruptMarker  MXFTag = 0x0f01 // test-only: force parse failure
)

// mxfValueType discriminates the encoded payload of a local tag.
type mxfValueType uint8

const (
	mxfU32 mxfValueType = iota + 1
	mxfU64
	mxfString
	mxfPair32
	mxfCrop32
	mxfBytes
)

// mxfField is one decoded local-tag entry.
type mxfField struct {
	Tag   MXFTag
	Type  mxfValueType
	U32   uint32
	U64   uint64
	Str   string
	Pair  [2]uint32
	Crop  Crop
	Bytes []byte
}

// MXFTrack is the decoded body of one essence-track local set.
type MXFTrack struct {
	ID     uint32
	Name   string
	Fields []mxfField
}

// MXFFile is the decoded, fixture-level MXF representation.
type MXFFile struct {
	HeaderFields []mxfField // preface/local set outside tracks
	Tracks       []MXFTrack
}

// Field returns the first field carrying tag within a field slice.
func findField(fields []mxfField, tag MXFTag) (*mxfField, bool) {
	for i := range fields {
		if fields[i].Tag == tag {
			return &fields[i], true
		}
	}
	return nil, false
}
