// Package model defines the shared error model and data contracts
// used by every subsystem of the Matroska/WebM archival pipeline.
package model

import "fmt"

// Severity classifies an archive condition.
type Severity string

const (
	// SeverityError blocks writing unless an explicit recovery policy applies.
	SeverityError Severity = "error"
	// SeverityWarning is carried into the manifest but never silently fixed.
	SeverityWarning Severity = "warning"
)

// ArchiveError is the single error model shared by parser, mapping, remux
// and recovery subsystems. Offset is the zero-based source byte position the
// condition was detected at; -1 means the condition is not byte anchored.
type ArchiveError struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Offset   int64    `json:"offset"`
}

// Error implements the error interface.
func (e *ArchiveError) Error() string {
	if e.Offset >= 0 {
		return fmt.Sprintf("%s: %s (offset=%d)", e.Code, e.Message, e.Offset)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError builds a byte-anchored hard error.
func NewError(code, message string, offset int64) *ArchiveError {
	return &ArchiveError{Code: code, Severity: SeverityError, Message: message, Offset: offset}
}

// NewWarning builds a non-fatal, manifest-bound warning.
func NewWarning(code, message string, offset int64) *ArchiveError {
	return &ArchiveError{Code: code, Severity: SeverityWarning, Message: message, Offset: offset}
}

// Known EBML element identifiers used by the archival pipeline.
const (
	IDEBMLHeader       = 0x1A45DFA3
	IDSegment          = 0x18538067
	IDSegmentInfo      = 0x1549A966
	IDTimestampScale   = 0x2AD7B1
	IDMuxingApp        = 0x4D80
	IDWritingApp       = 0x5741
	IDDuration         = 0x4489
	IDTracks           = 0x1654AE6B
	IDTrackEntry       = 0xAE
	IDTrackNumber      = 0xD7
	IDTrackUID         = 0x73C5
	IDTrackType        = 0x83
	IDCodecID          = 0x86
	IDDefaultDuration  = 0x23E383
	IDTrackOperation   = 0xE2
	IDTrackJoinBlocks  = 0xE9
	IDTrackJoinUID     = 0xED
	IDAudio            = 0xE1
	IDChannels         = 0x9F
	IDBitDepth         = 0x6264
	IDSamplingFreq     = 0xB5
	IDChannelPositions = 0x7D7B
	IDVideo            = 0xE0
	IDPixelWidth       = 0xB0
	IDPixelHeight      = 0xBA
	IDColour           = 0x55B0
	IDColorMatrix      = 0x55B1
	IDColorRange       = 0x55B9
	IDColorPrimaries   = 0x55BB
	IDTransferChar     = 0x55BA
	IDCluster          = 0x1F43B675
	IDClusterTimecode  = 0xE7
	IDSimpleBlock      = 0xA3
	IDBlockGroup       = 0xA0
	IDBlock            = 0xA1
)

// TrackType is the Matroska track type enumeration we archive.
type TrackType uint8

const (
	TrackTypeVideo    TrackType = 1
	TrackTypeAudio    TrackType = 2
	TrackTypeSubtitle TrackType = 17
)
