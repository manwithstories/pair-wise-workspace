package ebml

// Element IDs used by the parser (Matroska/WebM).
const (
	IDEBML            = 0x1A45DFA3
	IDDocType         = 0x4282
	IDSegment         = 0x18538067
	IDInfo            = 0x1549A966
	IDTimecodeScale   = 0x2AD7B1
	IDDuration        = 0x4489
	IDTracks          = 0x1654AE6B
	IDTrackEntry      = 0xAE
	IDTrackNumber     = 0xD7
	IDTrackUID        = 0x73C5
	IDTrackType       = 0x83
	IDCodecID         = 0x86
	IDTrackName       = 0x536E
	IDDefaultDuration = 0x23E383
	IDCluster         = 0x1F43B675
	IDTimecode        = 0xE7
	IDSimpleBlock     = 0xA3
	IDBlockGroup      = 0xA0
	IDBlock           = 0xA1
	IDBlockDuration   = 0x9B
	IDReferenceBlock  = 0xFB
	IDCues            = 0x1C53BB6B
	// Private extension used by fixtures to declare cross-track dependencies
	// (a cue track authored against a video track, for example).
	IDDependsOn = 0x4EC7
)
