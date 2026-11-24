package pipeline

import (
	"mediad/internal/dsp"
	"mediad/internal/fault"
	"mediad/internal/reorder"
)

// Config controls one pipeline invocation.
type Config struct {
	InputPath            string
	IndexPath            string
	OutputPath           string
	WorkDir              string
	TxID                 string
	Rate                 int
	Channels             int
	PacketFrames         int
	InjectStage          string
	ExpectedDamageOffset int64
}

// CaptureReport is the checksummed capture-stage summary.
type CaptureReport struct {
	Records           int            `json:"records"`
	IndexRebuilt      bool           `json:"index_rebuilt"`
	Damages           []fault.Damage `json:"damages"`
	DamageOffset      int64          `json:"damage_offset"`
	DamageOffsetError int64          `json:"damage_offset_error_bytes"`
	Stats             reorder.Stats  `json:"stats"`
	MissingFrames     int            `json:"missing_frames"`
	PacketFrames      int            `json:"packet_frames"`
	Rate              int            `json:"rate"`
	Channels          int            `json:"channels"`
}

// Result contains terminal metrics and the committed output identity.
type Result struct {
	Rollbacks         int
	IndexRebuilt      bool
	Records           int
	DamageOffset      int64
	DamageOffsetError int64
	Reorder           reorder.Stats
	MissingFrames     int
	Samples           int
	SHA256            string
	Bytes             int64
	Loudness          dsp.LoudnessReport
	InjectedExit      bool
}
