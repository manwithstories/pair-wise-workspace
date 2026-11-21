package probe

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// UL is a 16-byte SMPTE universal label.
type UL [16]byte

// ParseUL parses the dotted hex form "060e2b34.02530101.0d010101.01012f00".
func ParseUL(s string) (UL, error) {
	var u UL
	clean := strings.ReplaceAll(s, ".", "")
	if len(clean) != 32 {
		return u, fmt.Errorf("UL %q must be 32 hex digits", s)
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return u, fmt.Errorf("UL %q: %w", s, err)
	}
	copy(u[:], b)
	return u, nil
}

// MustUL is ParseUL for constants.
func MustUL(s string) UL {
	u, err := ParseUL(s)
	if err != nil {
		panic(err)
	}
	return u
}

// String renders the dotted form used by SMPTE documentation.
func (u UL) String() string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "." + h[8:16] + "." + h[16:24] + "." + h[24:32]
}

// KLV keys used by the mediad MXF profile. The profile is a strict subset of
// the SMPTE 377M KLV/local-set encoding: partition packs, a primer pack,
// structural metadata sets, one essence element per track and an index table.
// Every set and property is resolved through its UL exactly as a full MXF
// reader would, so nothing is located by fixed position.
var (
	ULHeaderPartition = MustUL("060e2b34.02050101.0d010201.01020100")
	ULBodyPartition   = MustUL("060e2b34.02050101.0d010201.01020300")
	ULFooterPartition = MustUL("060e2b34.02050101.0d010201.01020400")
	ULPrimerPack      = MustUL("060e2b34.02050101.0d010201.01050100")
	ULIndexTable      = MustUL("060e2b34.02050101.0d010201.01110100")

	ULPreface           = MustUL("060e2b34.02530101.0d010101.01012f00")
	ULContentStorage    = MustUL("060e2b34.02530101.0d010101.01011800")
	ULSourcePackage     = MustUL("060e2b34.02530101.0d010101.01033600")
	ULTimelineTrack     = MustUL("060e2b34.02530101.0d010101.01031200")
	ULSequence          = MustUL("060e2b34.02530101.0d010101.01010f00")
	ULSourceClip        = MustUL("060e2b34.02530101.0d010101.01011100")
	ULTimecodeComponent = MustUL("060e2b34.02530101.0d010101.01011400")
	ULCDCIDescriptor    = MustUL("060e2b34.02530101.0d010101.01012800")
	ULWaveDescriptor    = MustUL("060e2b34.02530101.0d010101.01012900")
)

// Essence element keys identify the element type in bytes 12-13 and carry
// the track number in their last two bytes.
var (
	essencePictureBase = MustUL("060e2b34.01010202.0d010301.14010000")
	essenceSoundBase   = MustUL("060e2b34.01010202.0d010301.15010000")
)

// Data definition labels.
var (
	ULDataDefPicture = MustUL("060e2b34.04010101.0d010201.01010100")
	ULDataDefSound   = MustUL("060e2b34.04010101.0d010201.01020200")
)

// essenceKey returns the element key for a track number.
func essenceKey(kind keyKind, trackNo uint32) UL {
	k := essencePictureBase
	if kind == keySound {
		k = essenceSoundBase
	}
	if trackNo == 0 || trackNo > 0xffff {
		panic(fmt.Sprintf("essence track number %d out of range", trackNo))
	}
	k[14] = byte(trackNo >> 8)
	k[15] = byte(trackNo)
	return k
}

// trackFromEssenceKey returns the track number encoded in an essence key.
func trackFromEssenceKey(u UL) (uint32, bool) {
	base := u
	base[14], base[15] = 0, 0
	if base != essencePictureBase && base != essenceSoundBase {
		return 0, false
	}
	n := uint32(u[14])<<8 | uint32(u[15])
	return n, n != 0
}

type keyKind int

const (
	keyPicture keyKind = iota
	keySound
)

// PropType is how a local-set property value is decoded.
type PropType int

const (
	// PropRaw leaves the bytes uninterpreted.
	PropRaw PropType = iota
	// PropUint32 is a 4-byte big-endian unsigned integer.
	PropUint32
	// PropInt32 is a 4-byte big-endian signed integer.
	PropInt32
	// PropUint64 is an 8-byte big-endian unsigned integer.
	PropUint64
	// PropRational is a pair of 4-byte signed integers.
	PropRational
	// PropUL is a 16-byte universal label.
	PropUL
	// PropUMID is a 32-byte unique material identifier.
	PropUMID
	// PropStrongRef is a 16-byte UL + 32-byte UMID + 4-byte track id.
	PropStrongRef
	// PropStrongRefArray is a u32 count of concatenated strong refs.
	PropStrongRefArray
	// PropString is a UTF-16BE string.
	PropString
	// PropUint8Array is a raw byte array.
	PropUint8Array
)

// Prop describes one property in the profile's primer.
type Prop struct {
	UL   UL
	Name string
	Type PropType
}

// mxfCatalog is the profile's complete tag catalogue. Fixtures write exactly
// these tags and the reader resolves each tag back through this table.
var mxfCatalog = []struct {
	Tag  uint16
	Prop Prop
}{
	{0x3b09, Prop{MustUL("060e2b34.01010101.0d010101.01010500"), "OperationalPattern", PropUL}},
	{0x1901, Prop{MustUL("060e2b34.01010101.0d010101.01012000"), "ContentStorageRef", PropStrongRef}},
	{0x1902, Prop{MustUL("060e2b34.01010101.0d010101.01011900"), "Packages", PropStrongRefArray}},

	{0x4401, Prop{MustUL("060e2b34.01010101.0d010101.01014400"), "PackageUMID", PropUMID}},
	{0x4402, Prop{MustUL("060e2b34.01010101.0d010101.01014405"), "PackageName", PropString}},
	{0x1502, Prop{MustUL("060e2b34.01010101.0d010101.01011500"), "Tracks", PropStrongRefArray}},
	{0x4801, Prop{MustUL("060e2b34.01010101.0d010101.01014800"), "TrackID", PropUint32}},
	{0x4b01, Prop{MustUL("060e2b34.01010101.0d010101.01014b00"), "EditRate", PropRational}},
	{0x0202, Prop{MustUL("060e2b34.01010101.0d010101.01010200"), "Duration", PropUint64}},
	{0x1201, Prop{MustUL("060e2b34.01010101.0d010101.01011200"), "StartPosition", PropUint64}},
	{0x0201, Prop{MustUL("060e2b34.01010101.0d010101.01012001"), "DataDefinition", PropUL}},
	{0x1501, Prop{MustUL("060e2b34.01010101.0d010101.01011501"), "DescriptorRef", PropStrongRef}},
	{0x1610, Prop{MustUL("060e2b34.01010101.0d010101.01011610"), "TimecodeRef", PropStrongRef}},
	{0x1002, Prop{MustUL("060e2b34.01010101.0d010101.01011002"), "SequenceRef", PropStrongRef}},
	{0x1001, Prop{MustUL("060e2b34.01010101.0d010101.01011000"), "Components", PropStrongRefArray}},
	{0x1101, Prop{MustUL("060e2b34.01010101.0d010101.01011100"), "SourceTrackID", PropUint32}},
	{0x1301, Prop{MustUL("060e2b34.01010101.0d010101.01011301"), "StartTimecode", PropUint64}},
	{0x1302, Prop{MustUL("060e2b34.01010101.0d010101.01011302"), "RoundedTimecodeBase", PropUint32}},
	{0x1303, Prop{MustUL("060e2b34.01010101.0d010101.01011303"), "DropFrame", PropUint32}},

	{0x3201, Prop{MustUL("060e2b34.01010101.0d010101.01013201"), "StoredWidth", PropUint32}},
	{0x3202, Prop{MustUL("060e2b34.01010101.0d010101.01013202"), "StoredHeight", PropUint32}},
	{0x3203, Prop{MustUL("060e2b34.01010101.0d010101.01013203"), "SampledWidth", PropUint32}},
	{0x3204, Prop{MustUL("060e2b34.01010101.0d010101.01013204"), "SampledHeight", PropUint32}},
	{0x3206, Prop{MustUL("060e2b34.01010101.0d010101.01013206"), "AspectRatio", PropRational}},
	{0x3207, Prop{MustUL("060e2b34.01010101.0d010101.01013207"), "GOPLength", PropUint32}},
	{0x3210, Prop{MustUL("060e2b34.01010101.0d010101.01013210"), "ColorPrimaries", PropUint32}},
	{0x3211, Prop{MustUL("060e2b34.01010101.0d010101.01013211"), "TransferCharacteristic", PropUint32}},
	{0x3212, Prop{MustUL("060e2b34.01010101.0d010101.01013212"), "MatrixCoefficient", PropUint32}},
	{0x3213, Prop{MustUL("060e2b34.01010101.0d010101.01013213"), "FullRangeFlag", PropUint32}},
	{0x3214, Prop{MustUL("060e2b34.01010101.0d010101.01013214"), "HorizCrop", PropUint32}},
	{0x3215, Prop{MustUL("060e2b34.01010101.0d010101.01013215"), "VertCrop", PropUint32}},
	{0x3216, Prop{MustUL("060e2b34.01010101.0d010101.01013216"), "RotationDegrees", PropInt32}},

	{0x3d01, Prop{MustUL("060e2b34.01010101.0d010101.01013d01"), "AudioSamplingRate", PropRational}},
	{0x3d02, Prop{MustUL("060e2b34.01010101.0d010101.01013d02"), "ChannelCount", PropUint32}},
	{0x3d03, Prop{MustUL("060e2b34.01010101.0d010101.01013d03"), "QuantizationBits", PropUint32}},

	{0x3f01, Prop{MustUL("060e2b34.01010101.0d010101.01013f01"), "IndexEditRate", PropRational}},
	{0x3f02, Prop{MustUL("060e2b34.01010101.0d010101.01013f02"), "IndexStartPosition", PropUint64}},
	{0x3f03, Prop{MustUL("060e2b34.01010101.0d010101.01013f03"), "IndexDuration", PropUint64}},
	{0x3f04, Prop{MustUL("060e2b34.01010101.0d010101.01013f04"), "EditUnitByteCount", PropUint32}},
	{0x3f05, Prop{MustUL("060e2b34.01010101.0d010101.01013f05"), "SliceCount", PropUint32}},
	{0x3f06, Prop{MustUL("060e2b34.01010101.0d010101.01013f06"), "IndexFlagsPerEditUnit", PropUint8Array}},
}

// primer maps a local tag to its property, as read from the file's primer.
type primer map[uint16]Prop

func defaultPrimer() primer {
	p := make(primer, len(mxfCatalog))
	for _, e := range mxfCatalog {
		p[e.Tag] = e.Prop
	}
	return p
}

// tagFor returns the wire tag for a property name.
func tagFor(name string) (uint16, bool) {
	for _, e := range mxfCatalog {
		if e.Prop.Name == name {
			return e.Tag, true
		}
	}
	return 0, false
}

func lookupTag(tag uint16) (Prop, bool) {
	for _, e := range mxfCatalog {
		if e.Tag == tag {
			return e.Prop, true
		}
	}
	return Prop{}, false
}
