package ts

import (
	"fmt"

	"cinepipe/internal/fault"
)

// Channel layout labels used in track descriptions. A description is kept
// verbatim per segment rather than normalised, so a stereo camera and a 5.1
// camera can coexist in one manifest without either being silently rewritten.
const (
	LayoutMono   = "mono"
	LayoutStereo = "stereo"
	Layout51     = "5.1"
	Layout71     = "7.1"
)

// AudioTrack is the immutable description of an audio stream: what a mixing
// tool needs in order to refuse to merge incompatible sources.
type AudioTrack struct {
	PID         uint16 `json:"pid"`
	StreamType  uint8  `json:"stream_type"`
	Codec       string `json:"codec"`
	SampleRate  int    `json:"sample_rate"`
	Channels    int    `json:"channels"`
	Layout      string `json:"layout"`
	Language    string `json:"language,omitempty"`
	FirstOffset int64  `json:"first_offset"`
}

// IsZero reports whether no audio was ever described.
func (a AudioTrack) IsZero() bool { return a.PID == 0 && a.Codec == "" }

// CompatibleWith reports whether two descriptions can be mixed without
// resampling or channel remapping. Language differences do not block mixing.
func (a AudioTrack) CompatibleWith(b AudioTrack) bool {
	return a.Codec == b.Codec && a.SampleRate == b.SampleRate && a.Channels == b.Channels
}

// DescribeReasons lists the properties that differ between two descriptions,
// in stable order, so a manifest can state exactly why tracks were kept apart.
func DescribeReasons(a, b AudioTrack) []string {
	var out []string
	if a.Codec != b.Codec {
		out = append(out, fmt.Sprintf("codec %s vs %s", a.Codec, b.Codec))
	}
	if a.SampleRate != b.SampleRate {
		out = append(out, fmt.Sprintf("sample_rate %d vs %d", a.SampleRate, b.SampleRate))
	}
	if a.Channels != b.Channels {
		out = append(out, fmt.Sprintf("channels %d vs %d", a.Channels, b.Channels))
	}
	if a.Layout != b.Layout {
		out = append(out, fmt.Sprintf("layout %s vs %s", a.Layout, b.Layout))
	}
	return out
}

// layoutForChannels maps a channel count onto a familiar layout label.
func layoutForChannels(n int) string {
	switch n {
	case 1:
		return LayoutMono
	case 2:
		return LayoutStereo
	case 6:
		return Layout51
	case 8:
		return Layout71
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%dch", n)
}

// ADTS sample-rate table, indexed by the 4-bit sampling_frequency_index.
var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// ADTS profile numbers map to AAC object names; cinepipe reports the profile
// without pretending to know LC vs HE details.
var adtsProfiles = [4]string{"main", "lc", "ssr", "ltp"}

// ADTSFrame is one parsed AAC ADTS frame.
type ADTSFrame struct {
	Profile    string
	SampleRate int
	Channels   int
	FrameLen   int
	HeaderLen  int
	HasCRC     bool
}

// ParseADTSHeader decodes an ADTS frame header at the head of b. It returns a
// diagnostic when the syncword, index, or frame length is impossible.
func ParseADTSHeader(b []byte) (ADTSFrame, bool, string) {
	var f ADTSFrame
	if len(b) < 7 {
		return f, false, "ADTS header truncated"
	}
	if b[0] != 0xFF || b[1]&0xF0 != 0xF0 {
		return f, false, fmt.Sprintf("bad ADTS syncword %02x%02x", b[0], b[1])
	}
	protectionAbsent := b[1]&0x01 != 0
	profile := (b[2] >> 6) & 0x03
	sfi := int(b[2] >> 2 & 0x0F)
	if sfi >= len(adtsSampleRates) || adtsSampleRates[sfi] == 0 {
		return f, false, fmt.Sprintf("invalid sampling_frequency_index %d", sfi)
	}
	ch := int(b[2]&0x01)<<2 | int(b[3]>>6)&0x03
	frameLen := int(b[3]&0x03)<<11 | int(b[4])<<3 | int(b[5]>>5&0x07)
	hdr := 7
	if !protectionAbsent {
		hdr = 9
	}
	if frameLen < hdr {
		return f, false, fmt.Sprintf("ADTS frame_length %d shorter than header %d", frameLen, hdr)
	}
	f.Profile = adtsProfiles[profile]
	f.SampleRate = adtsSampleRates[sfi]
	f.Channels = ch
	f.FrameLen = frameLen
	f.HeaderLen = hdr
	f.HasCRC = !protectionAbsent
	return f, true, ""
}

// BuildADTSHeader lays out a 7-byte ADTS header for one frame. It backs the
// sample generator so the fixture and parser stay in lockstep.
func BuildADTSHeader(profile uint8, sampleRateIndex uint8, channels uint8, frameLen int) []byte {
	b := make([]byte, 7)
	b[0] = 0xFF
	b[1] = 0xF1 // MPEG-4, layer 0, no CRC
	b[2] = (profile&0x03)<<6 | (sampleRateIndex&0x0F)<<2 | (channels>>2)&0x01
	b[3] = (channels&0x03)<<6 | uint8(frameLen>>11)&0x03
	b[4] = uint8(frameLen >> 3)
	b[5] = uint8(frameLen&0x07)<<5 | 0x1F
	b[6] = 0xFC
	return b
}

// SampleRateIndex returns the ADTS index for a rate, or -1 if unsupported.
func SampleRateIndex(rate int) int {
	for i, r := range adtsSampleRates {
		if r == rate {
			return i
		}
	}
	return -1
}

// AudioAssembly collects the first decodable frame of an audio PID and derives
// its description. Once a description is established it is frozen: later frames
// that disagree are reported as metadata differences rather than applied.
type AudioAssembly struct {
	PID    uint16
	Type   uint8
	Lang   string
	desc   AudioTrack
	locked bool
	frames int64
	first  int64
}

// NewAudioAssembly returns an assembly for one audio PID.
func NewAudioAssembly(pid uint16, streamType uint8, lang string) *AudioAssembly {
	return &AudioAssembly{PID: pid, Type: streamType, Lang: lang}
}

// Describe ingests one PES payload. It returns the track description the first
// time a valid frame header is seen, then nil while the description is stable.
// A conflict yields both the locked description and a metadata diagnostic.
func (a *AudioAssembly) Describe(payload []byte, off int64) (AudioTrack, bool, []fault.Error) {
	if a.locked {
		// Verify the stream has not silently changed parameters.
		if cand, ok, _ := a.parse(payload); ok {
			if cand.SampleRate != a.desc.SampleRate || cand.Channels != a.desc.Channels {
				return a.desc, true, []fault.Error{{Kind: fault.KindMetadata, Offset: off, PID: a.PID,
					Detail: fmt.Sprintf("audio parameters changed after lock: %dHz/%dch -> %dHz/%dch",
						a.desc.SampleRate, a.desc.Channels, cand.SampleRate, cand.Channels)}}
			}
		}
		return a.desc, false, nil
	}
	cand, ok, reason := a.parse(payload)
	if !ok {
		// Not a decodable frame: the description stays unknown until one arrives.
		return AudioTrack{}, false, []fault.Error{{Kind: fault.KindTruncated, Offset: off, PID: a.PID,
			Detail: "audio frame header: " + reason}}
	}
	a.desc = cand
	a.first = off
	a.locked = true
	return a.desc, true, nil
}

// parse decodes the description carried by a PES payload for the stream type.
func (a *AudioAssembly) parse(payload []byte) (AudioTrack, bool, string) {
	switch a.Type {
	case StreamTypeAAC:
		f, ok, reason := ParseADTSHeader(payload)
		if !ok {
			return AudioTrack{}, false, reason
		}
		return AudioTrack{
			PID: a.PID, StreamType: a.Type, Codec: "aac",
			SampleRate: f.SampleRate, Channels: f.Channels,
			Layout: layoutForChannels(f.Channels), Language: a.Lang, FirstOffset: a.first,
		}, true, ""
	case StreamTypeAC3, StreamTypeEAC3:
		return a.parseAC3(payload)
	case StreamTypeMP3:
		return AudioTrack{PID: a.PID, StreamType: a.Type, Codec: "mp3",
			SampleRate: 48000, Channels: 2, Layout: LayoutStereo, Language: a.Lang}, true, ""
	}
	return AudioTrack{}, false, "unsupported audio stream type"
}

// parseAC3 reads the bit-stream information fields of an AC-3 frame header.
// eac3 shares the syncword and sample-rate layout for the fields cinepipe uses.
func (a *AudioAssembly) parseAC3(b []byte) (AudioTrack, bool, string) {
	if len(b) < 6 {
		return AudioTrack{}, false, "AC-3 header truncated"
	}
	if b[0] != 0x0B || b[1] != 0x77 {
		return AudioTrack{}, false, fmt.Sprintf("bad AC-3 syncword %02x%02x", b[0], b[1])
	}
	// fscod: bits 7..6 of byte 4; acmod: bits 7..5 of byte 5.
	fscod := int(b[4] >> 6)
	rates := [4]int{48000, 44100, 32000, 0}
	if rates[fscod] == 0 {
		return AudioTrack{}, false, "reserved AC-3 fscod"
	}
	acmod := int(b[5] >> 5)
	channels := []int{2, 1, 2, 3, 3, 4, 4, 5}[acmod]
	if b[5]&0x10 != 0 { // lfeon adds a channel
		channels++
	}
	return AudioTrack{
		PID: a.PID, StreamType: a.Type, Codec: "ac3",
		SampleRate: rates[fscod], Channels: channels,
		Layout: layoutForChannels(channels), Language: a.Lang,
	}, true, ""
}

// Description returns the locked description, or the zero value.
func (a *AudioAssembly) Description() AudioTrack { return a.desc }

// Locked reports whether a description has been established.
func (a *AudioAssembly) Locked() bool { return a.locked }
