// Package metadata parses the five timed-event flavors carried by the
// drill footage: subtitles, stage chapters, ID3 tags, DASH emsg boxes and
// H.264/H.265 SEI NAL units. Every parser reports faults with the source
// track name and the exact byte offset inside the source blob.
package metadata

// ParserVersion is bumped whenever a parsing rule changes. Checkpoints
// store it so an old checkpoint is never reused against new parser logic.
const ParserVersion = "fp-parser-1"

// EventKind identifies a timed-event family.
type EventKind string

const (
	KindSubtitle EventKind = "subtitle"
	KindChapter  EventKind = "chapter"
	KindID3      EventKind = "id3"
	KindEMSG     EventKind = "emsg"
	KindSEI      EventKind = "sei"
)

// TimedEvent is the normalized representation shared by all five parsers.
// Position is the event start expressed in source units; Track is the
// originating stream. Offset is the byte offset of the event inside its
// source blob, used in every fault report.
type TimedEvent struct {
	Kind     EventKind
	Track    string
	Offset   int64
	Position int64
	Duration int64
	RateNum  int64
	RateDen  int64
	ID       string
	Payload  []byte
}

// nalEmulationPrevention removes the 0x03 byte inserted by the Annex-B
// encoder after every run of at least two zero bytes. It is the exact
// inverse of the encoding rule, so runs such as 00 00 00 01 cannot be
// mistaken for start codes inside a payload.
func nalEmulationPrevention(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp))
	zeros := 0
	for i := 0; i < len(rbsp); i++ {
		b := rbsp[i]
		if zeros >= 2 && b == 0x03 {
			// The 0x03 itself is not a zero. The following byte is the first
			// byte of the continuation run; when it is zero it counts once,
			// so pre-seed the counter at 1 and let the normal path take over.
			zeros = 1
			continue
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
