// Package media defines the shared domain model of avpipe: frames, audio
// packets, the 90 kHz media clock and the error model used by every module.
package media

// ErrorClass identifies the category of a pipeline error. The same taxonomy
// is used on the terminal output and in exit-code mapping.
type ErrorClass string

const (
	ClassValidation ErrorClass = "validation" // manifest / metadata rejected
	ClassCorrupt    ErrorClass = "corrupt"    // checksum or payload mismatch
	ClassDuplicate  ErrorClass = "duplicate"  // conflicting duplicate packet
	ClassLoss       ErrorClass = "loss"       // unrecoverable packet loss -> gap
	ClassClock      ErrorClass = "clock"      // timestamp wraparound / drift
	ClassEncode     ErrorClass = "encode"     // FFmpeg or bitrate failure
	ClassIndex      ErrorClass = "index"      // journal / checkpoint conflict
	ClassConfig     ErrorClass = "config"     // invalid runtime parameters
)

// Error is the common error model. Every failure carries its origin
// component, the 90 kHz timestamp it relates to (zero when not applicable)
// and whether the pipeline could recover from it.
type Error struct {
	Class       ErrorClass
	Source      string // component that produced the error, e.g. "ingest"
	PTS         uint64 // 90 kHz media timestamp, 0 when unrelated
	Recoverable bool
	Message     string
}

func (e *Error) Error() string {
	rec := "fatal"
	if e.Recoverable {
		rec = "recoverable"
	}
	return string(e.Class) + " error from " + e.Source + " at pts=" +
		itoa(e.PTS) + " (" + rec + "): " + e.Message
}

// NewError builds a media.Error.
func NewError(class ErrorClass, source, message string, pts uint64, recoverable bool) *Error {
	return &Error{Class: class, Source: source, PTS: pts, Recoverable: recoverable, Message: message}
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// Frame is one raw video frame in planar YUV 4:2:0 (I420). Width/Height
// describe the source resolution; encoders downscale per representation.
type Frame struct {
	Seq    uint32 // frame sequence number within the asset
	PTS    uint64 // presentation timestamp on the 90 kHz clock
	Width  int
	Height int
	Y      []byte // luma plane, size Width*Height
	Cb     []byte // chroma U plane, size (Width/2)*(Height/2)
	Cr     []byte // chroma V plane, size (Width/2)*(Height/2)
	SHA256 []byte // expected checksum of the packed YUV bytes
	Gapped bool   // true when this frame is a freeze-fill placeholder
}

// AudioPacket is one chunk of 48 kHz, mono, 16-bit little-endian PCM.
type AudioPacket struct {
	Seq     uint32
	PTS     uint64 // first sample's timestamp on the shared 90 kHz clock
	Samples int    // number of mono samples
	PCM     []byte // Samples*2 bytes, little endian
	SHA256  []byte
	Gapped  bool // true when filled with silence after loss
}

// PacketKind discriminates transport packets.
type PacketKind uint8

const (
	KindVideo  PacketKind = 1
	KindAudio  PacketKind = 2
	KindParity PacketKind = 3 // XOR FEC packet for one 8-member block
)

// Packet is the universal transport unit. Video frames and audio packets are
// carried as payload; parity packets carry the XOR of their block.
type Packet struct {
	Seq       uint32 // global transport sequence number
	BlockID   uint32 // FEC block (Seq/8 for media packets)
	Kind      PacketKind
	ParityOf  PacketKind // for KindParity: the media stream it protects
	PTS       uint64
	Payload   []byte
	SHA256    []byte // checksum of Payload
	Recovered bool   // reconstructed by FEC, not received intact
	Corrupt   bool   // digest mismatch at ingest; bytes must not be trusted
}

// ParitySize returns the XOR length used for a block: the maximum payload
// length of its members, zero-length packets participating as zeros.
func ParitySize(members [][]byte) int {
	n := 0
	for _, m := range members {
		if len(m) > n {
			n = len(m)
		}
	}
	return n
}
