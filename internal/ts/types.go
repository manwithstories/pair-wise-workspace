package ts

// Kind identifies the elementary stream type.
type Kind int

const (
	KindUnknown Kind = iota
	KindVideo
	KindAudio
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	}
	return "unknown"
}

// AccessUnit is one complete PES packet: a video access unit or an AAC frame
// group. PTS and DTS are unwrapped 27 MHz ticks and are -1 when absent.
type AccessUnit struct {
	PID           uint16
	Kind          Kind
	PTS           int64
	DTS           int64
	HasPTS        bool
	HasDTS        bool
	Keyframe      bool
	Data          []byte
	FirstPacket   int
	LastPacket    int
	Unrecoverable bool
	Reason        string
}

type pesAccum struct {
	pid    uint16
	kind   Kind
	data   []byte
	first  int
	last   int
	broken bool
	reason string
	hdr    *PESHeader
}
