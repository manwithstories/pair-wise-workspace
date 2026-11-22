// Package defect defines the error model shared by every subsystem:
// each defect is located by track, packet sequence number, source byte
// offset and a category.
package defect

// Category classifies a defect.
type Category string

// Defect categories propagated across subsystems.
const (
	Duplicate         Category = "duplicate"
	Reordered         Category = "reordered"
	Loss              Category = "loss"
	LatePacket        Category = "late_packet"
	Malformed         Category = "malformed"
	SubtitleParse     Category = "subtitle_parse"
	TimecodeConflict  Category = "timecode_conflict"
	SpanChunk         Category = "span_chunk"
	IndexCorrupt      Category = "index_corrupt"
	OutputCorrupt     Category = "output_corrupt"
)

// Defect is a single traceable problem.
type Defect struct {
	Track    string   `json:"track"`
	Seq      int64    `json:"seq"`
	Offset   int64    `json:"offset_bytes"`
	Category Category `json:"category"`
	Detail   string   `json:"detail"`
}

// New builds a Defect. Use -1 for seq or offset when not applicable.
func New(track string, seq, offset int64, cat Category, detail string) Defect {
	return Defect{Track: track, Seq: seq, Offset: offset, Category: cat, Detail: detail}
}
