package ebml

// Matroska/WebM element IDs used by the streaming parser and fixture writer.
const (
	IDEBMLHeader       uint32 = 0x1A45DFA3
	IDSegment          uint32 = 0x18538067
	IDSeekHead         uint32 = 0x114D9B74
	IDInfo             uint32 = 0x1549A966
	IDTracks           uint32 = 0x1654AE6B
	IDCluster          uint32 = 0x1F43B675
	IDTimecode         uint32 = 0xE7
	IDTimecodeScale    uint32 = 0x2AD7B1
	IDMuxingApp        uint32 = 0x4D80
	IDWritingApp       uint32 = 0x5741
	IDDuration         uint32 = 0x4489
	IDTrackEntry       uint32 = 0xAE
	IDTrackNumber      uint32 = 0xD7
	IDTrackUID         uint32 = 0x73C5
	IDTrackType        uint32 = 0x83
	IDCodecID          uint32 = 0x86
	IDAudio            uint32 = 0xE1
	IDSamplingFreq     uint32 = 0xB5
	IDChannels         uint32 = 0x9F
	IDBlockGroup       uint32 = 0xA0
	IDBlock            uint32 = 0xA1
	IDSimpleBlock      uint32 = 0xA3
	IDReferenceBlock   uint32 = 0xFB
	IDChapters         uint32 = 0x1043A770
	IDCues             uint32 = 0x1C53BB6B
	IDTags             uint32 = 0x1254C367
	IDAttachments      uint32 = 0x1941A469
)

// Track types from the Matroska specification.
const (
	TrackTypeVideo   = 1
	TrackTypeAudio   = 2
	TrackTypeComplex = 3
)

// Lacing flags in SimpleBlock/Block header byte.
const (
	LacingNone  = 0
	LacingXiph  = 2
	LacingFixed = 4
	LacingEBML  = 6
	LacingMask  = 0x06
)
