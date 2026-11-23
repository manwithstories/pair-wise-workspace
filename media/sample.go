package media

// TrackKind distinguishes elementary streams.
type TrackKind int

const (
	TrackVideo TrackKind = iota
	TrackAudio
)

func (k TrackKind) String() string {
	if k == TrackVideo {
		return "video"
	}
	return "audio"
}

// KeyframeSource records how a keyframe flag was established.
type KeyframeSource string

const (
	// KeySrcSTSS: declared by the stss box of the container.
	KeySrcSTSS KeyframeSource = "stss"
	// KeySrcRebuilt: reconstructed by scanning sample payloads.
	KeySrcRebuilt KeyframeSource = "rebuilt"
	// KeySrcNone: no reliable keyframe information.
	KeySrcNone KeyframeSource = ""
)

// Sample is one access unit with its track-local timestamps and its byte
// provenance in the source file. Payload is not retained; readers fetch
// [Offset, Offset+Size) from the source on demand to bound memory.
type Sample struct {
	TrackID   int
	Kind      TrackKind
	DTS       Time // decode time, track timescale
	CTS       Time // composition time, track timescale
	Duration  uint32
	Offset    int64 // absolute offset of the sample payload in the source
	Size      uint32
	Keyframe  bool
	KeySource KeyframeSource
}

// End returns the DTS just past this sample.
func (s Sample) End() Time { return s.DTS.Add(int64(s.Duration)) }

// Range is a half-open byte interval [Start, End) of the source file,
// used to report damaged or unrecoverable regions.
type Range struct {
	Start  int64
	End    int64
	Reason string
}

// File is the demuxed view of one input: its tracks, all salvageable
// samples in decode order, and the damaged byte ranges encountered.
type File struct {
	Path    string
	Tracks  []Track
	Samples []Sample
	Damaged []Range
	Issues  []Issue
}

// IssueKind classifies a structural fault. Unlike Damaged, which always
// names a byte range that could not be salvaged, an Issue may describe a
// missing structure (e.g. no stss) without claiming any bytes are corrupt.
type IssueKind string

const (
	IssueTruncated       IssueKind = "truncated"
	IssueBrokenBox       IssueKind = "broken_box"
	IssueMissingMoov     IssueKind = "missing_moov"
	IssueIncompleteIndex IssueKind = "incomplete_index"
	IssueUnsupportedTrak IssueKind = "unsupported_track"
)

// Issue is one structural fault tied to an optional byte offset and track.
type Issue struct {
	Kind    IssueKind
	Offset  int64 // first affected offset, -1 when not applicable
	TrackID int
	Detail  string
}

// AddIssue appends an issue unless an identical one already exists; the
// parser can encounter the same fault from several sample tables.
func (f *File) AddIssue(iss Issue) {
	for _, e := range f.Issues {
		if e.Kind == iss.Kind && e.Offset == iss.Offset && e.TrackID == iss.TrackID {
			return
		}
	}
	f.Issues = append(f.Issues, iss)
}

// HasIssue reports whether any issue of kind k was recorded.
func (f *File) HasIssue(k IssueKind) bool {
	for _, e := range f.Issues {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// Track describes one elementary stream.
type Track struct {
	ID         int
	Kind       TrackKind
	Timescale  uint32
	Duration   int64          // in track timescale
	CodecEntry []byte         // raw stsd sample-description box, copied verbatim
	IndexFrom  KeyframeSource // how the keyframe index was established
}

// TrackByKind returns the first track of the given kind.
func (f *File) TrackByKind(k TrackKind) (Track, bool) {
	for _, t := range f.Tracks {
		if t.Kind == k {
			return t, true
		}
	}
	return Track{}, false
}

// SamplesOf returns the samples of one track in decode order.
func (f *File) SamplesOf(trackID int) []Sample {
	var out []Sample
	for _, s := range f.Samples {
		if s.TrackID == trackID {
			out = append(out, s)
		}
	}
	return out
}
