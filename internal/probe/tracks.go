// Package probe validates the audio and video layout of parsed tracks. It
// never invents source metadata: values that cannot be interpreted are kept
// verbatim and reported as warnings, while conflicts are resolved only through
// an explicitly supplied policy.
package probe

import (
	"errors"
)

// Sentinel errors.
var (
	ErrRejected = errors.New("conflict rejected by policy")
	ErrNoTrack  = errors.New("representation has no usable track")
)

// Choice is an explicit normalisation policy value.
type Choice string

const (
	Reject    Choice = "reject"    // fail on conflict
	Primary   Choice = "primary"   // keep the first representation's value
	Mask      Choice = "mask"      // audio: prefer the channel mask
	Entry     Choice = "entry"     // audio: prefer the sample-entry count
	Timescale Choice = "timescale" // audio: prefer the media timescale
)

// Policy controls how conflicts are normalised.
type Policy struct {
	Channels   Choice
	SampleRate Choice
	Color      Choice
}

// DefaultPolicy normalises to the container-declared mask and sample entry,
// preferring the first representation for cross-representation conflicts.
func DefaultPolicy() Policy {
	return Policy{Channels: Mask, SampleRate: Entry, Color: Primary}
}

func (p Policy) withDefaults() Policy {
	if p.Channels == "" {
		p.Channels = Mask
	}
	if p.SampleRate == "" {
		p.SampleRate = Entry
	}
	if p.Color == "" {
		p.Color = Primary
	}
	return p
}

// Severity classifies a finding.
type Severity string

const (
	Warning Severity = "warning"
	Error   Severity = "error"
)

// Finding is a normalisation decision or an unresolvable value.
type Finding struct {
	Code     string
	Track    string
	Severity Severity
	Detail   string
}

// ColorInfo is the video colour description carried by a colr/nclx box.
type ColorInfo struct {
	Present   bool
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
	Complete  bool // all three codes were understood
}

// AudioInfo is the audio layout of one representation.
type AudioInfo struct {
	TrackID       uint32
	EntryChannels int
	Mask          uint64
	HasMask       bool
	LayoutCount   int // channelLayout entries in the source
	SampleRate    int
	Timescale     int
	ObjectType    uint8
	HasObjectType bool
}

// VideoInfo is the video layout of one representation.
type VideoInfo struct {
	TrackID uint32
	Width   int
	Height  int
	Color   ColorInfo
}

// Representation is one rendition as parsed from the container.
type Representation struct {
	ID    string
	Audio *AudioInfo
	Video *VideoInfo
}

// AudioResult is the normalised audio layout.
type AudioResult struct {
	TrackID         uint32
	Channels        int
	Mask            uint64
	SampleRate      int
	SampleRateKnown bool
	ObjectType      uint8
	ObjectTypeKnown bool
}

// VideoResult is the normalised video layout.
type VideoResult struct {
	TrackID    uint32
	Color      ColorInfo
	ColorKnown bool
}

// Report collects findings and the normalised values.
type Report struct {
	Audio    []AudioResult
	Video    []VideoResult
	Findings []Finding
}

// Failed reports whether any finding is an error.
func (r Report) Failed() bool {
	for _, f := range r.Findings {
		if f.Severity == Error {
			return true
		}
	}
	return false
}

// Codes returns the finding codes in order, for stable summaries.
func (r Report) Codes() []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Code)
	}
	return out
}

var knownAudioRates = map[int]bool{
	8000: true, 11025: true, 12000: true, 16000: true, 22050: true,
	24000: true, 32000: true, 44100: true, 48000: true, 96000: true,
}

var knownObjectTypes = map[uint8]bool{0x40: true, 0x66: true, 0x67: true, 0x68: true}

var knownPrimaries = map[uint16]bool{
	1: true, 4: true, 5: true, 6: true, 7: true, 8: true,
	9: true, 10: true, 11: true, 12: true, 22: true,
}
var knownTransfer = map[uint16]bool{
	1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true,
	10: true, 11: true, 12: true, 13: true, 14: true, 15: true,
	16: true, 17: true, 18: true,
}
var knownMatrix = map[uint16]bool{
	0: true, 1: true, 4: true, 5: true, 6: true, 7: true,
	8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true,
}

// Validate inspects every representation under the policy.
func Validate(reps []Representation, policy Policy) (Report, error) {
	policy = policy.withDefaults()
	var rep Report
	var audio *AudioResult
	var video *VideoResult
	for _, r := range reps {
		if r.Audio != nil {
			res, findings, err := normalizeAudio(r.Audio, policy)
			rep.Findings = append(rep.Findings, findings...)
			if err != nil {
				return rep, err
			}
			rep.Audio = append(rep.Audio, res)
			if audio == nil {
				a := res
				audio = &a
			}
		}
		if r.Video != nil {
			res, findings, err := normalizeVideo(r.Video, policy)
			rep.Findings = append(rep.Findings, findings...)
			if err != nil {
				return rep, err
			}
			rep.Video = append(rep.Video, res)
			if video == nil {
				v := res
				video = &v
			}
		}
	}
	if len(rep.Audio) == 0 && len(rep.Video) == 0 {
		return rep, ErrNoTrack
	}
	// Cross-representation reconciliation.
	if len(rep.Audio) > 1 {
		if err := reconcileAudio(&rep, policy); err != nil {
			return rep, err
		}
	}
	if len(rep.Video) > 1 {
		if err := reconcileVideo(&rep, policy); err != nil {
			return rep, err
		}
	}
	return rep, nil
}
