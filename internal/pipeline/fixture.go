// Package pipeline wires generated local fixtures through FieldCut's stages.
// It is the orchestration layer behind the terminal acceptance adapter.
package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/pion/rtp"

	"fieldcut/internal/audio"
	"fieldcut/internal/clock"
	"fieldcut/internal/events"
	"fieldcut/internal/ingest"
)

const (
	Duration     = 4 * clock.NsPerSecond
	VideoFPS     = 30
	FrameCount   = 120
	FramePayload = 4980
	TargetBPS    = 1_200_000
	OutputRate   = 48000
	DamageSkewNS = 80 * clock.NsPerMs
)

// Request is persisted so resume needs only --out.
type Request struct {
	Case      string `json:"case"`
	Seed      int64  `json:"seed"`
	OutDir    string `json:"out_dir"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

// Bundle contains deterministically generated source material.
type Bundle struct {
	DirectorBytes []byte
	StageBytes    []byte
	Director      *ingest.MP4Track
	Stage         *ingest.MP4Track
	Audio48Stereo []byte
	Audio44Mono   []byte
	Audio51       []byte
	RawEvents     []events.RawEvent
	RTPPackets    [][]byte
	RequestHash   string
}

// HashBytes returns the hex SHA-256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func patternPayload(seed int64, key uint32) []byte {
	b := make([]byte, FramePayload)
	binary.LittleEndian.PutUint32(b[0:4], key)
	binary.LittleEndian.PutUint64(b[4:12], uint64(seed))
	state := uint64(key)*1_000_003 + uint64(seed) + 17
	for i := 12; i < len(b); i++ {
		state = state*6364136223846793005 + 1442695040888963407
		b[i] = byte(state >> 33)
	}
	return b
}

func videoTracks(seed int64, damaged bool) (*ingest.MP4Track, *ingest.MP4Track, []byte, []byte, error) {
	director := &ingest.MP4Track{TrackID: 1, TimeScale: VideoFPS, Color: ingest.ColorInfo{
		Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", FullRange: false, HasMetadata: true,
	}}
	stage := &ingest.MP4Track{TrackID: 2, TimeScale: VideoFPS, Color: ingest.ColorInfo{
		Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", FullRange: true, HasMetadata: true,
	}}
	frameDur := clock.NsPerSecond / VideoFPS
	for i := 0; i < FrameCount; i++ {
		key := uint32(1000 + i)
		director.Samples = append(director.Samples, ingest.MP4Sample{PTS: clock.Ns(i) * frameDur, Duration: frameDur, Data: patternPayload(seed, key), Sync: i%30 == 0})

		stageKey := key + 5000
		if i >= 60 && i < 90 {
			// Exact offset duplicate of director frames 30..59.
			stageKey = uint32(1000 + (i - 30))
		}
		stage.Samples = append(stage.Samples, ingest.MP4Sample{PTS: clock.Ns(i) * frameDur, Duration: frameDur, Data: patternPayload(seed, stageKey), Sync: i%30 == 0})
	}
	db, err := ingest.BuildFragmentedMP4(director)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sb, err := ingest.BuildFragmentedMP4(stage)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if damaged {
		// Remove tail payload bytes while leaving box/trun sizes declared.
		db = db[:len(db)-300]
	}
	return director, stage, db, sb, nil
}

func pcmTone(rate, channels int, layout audio.Layout, seed int64, clips bool) []byte {
	frames := rate * int(Duration/clock.NsPerSecond)
	p := &audio.PCM{TrackID: channels*100 + rate, SampleRate: rate, Layout: layout, Channels: make([][]float64, channels)}
	for c := range p.Channels {
		p.Channels[c] = make([]float64, frames)
		for i := 0; i < frames; i++ {
			t := float64(i) / float64(rate)
			silence := t >= 1.8 && t < 2.2
			v := 0.0
			if !silence {
				v = 0.22 * sinWave(2*3.141592653589793*440*t+float64(c)*0.7)
			}
			p.Channels[c][i] = v
		}
	}
	if clips {
		for _, f := range []int{rate / 2, rate/2 + 1} {
			p.Channels[0][f] = 1
			if channels > 1 {
				p.Channels[1][f] = -1
			}
		}
	}
	return audio.AppendPCM16(p)
}

func sinWave(x float64) float64 {
	return math.Sin(x)
}

func rtpFixture() [][]byte {
	out := make([][]byte, 0, 12)
	seq := uint16(65530)
	ts := uint32(0xffffffe0)
	for i := 0; i < 12; i++ {
		// Deliberately swap two adjacent packets after the 32-bit wrap.
		orderI := i
		if i == 7 {
			orderI = 8
		} else if i == 8 {
			orderI = 7
		}
		b, _ := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq + uint16(orderI), Timestamp: ts + uint32(orderI*3000), SSRC: 17275},
			Payload: []byte(fmt.Sprintf("rtp-event-%d", orderI)),
		}).Marshal()
		out = append(out, b)
	}
	return out
}

func eventFixture(damaged bool) []events.RawEvent {
	ns := int64(clock.NsPerSecond)
	evs := []events.RawEvent{
		{ID: "chapter-1", TrackID: 40, Kind: events.KindChapter, TimeBaseDen: ns, StartTicks: ns, EndTicks: ns + 1, Label: "Verse"},
		{ID: "chapter-2", TrackID: 40, Kind: events.KindChapter, TimeBaseDen: ns, StartTicks: 2 * ns, EndTicks: 2*ns + 1, Label: "Chorus"},
		{ID: "chapter-3", TrackID: 40, Kind: events.KindChapter, TimeBaseDen: ns, StartTicks: 3 * ns, EndTicks: 3*ns + 1, Label: "Outro"},
		{ID: "sub-1", GroupID: "moment-1", TrackID: 30, Kind: events.KindSubtitle, TimeBaseDen: ns, StartTicks: int64(500 * clock.NsPerMs), EndTicks: int64(1200 * clock.NsPerMs), Label: "hello festival"},
		{ID: "id3-1", GroupID: "moment-1", TrackID: 31, Kind: events.KindID3, TimeBaseDen: ns, StartTicks: int64(500*clock.NsPerMs + 500_000), EndTicks: int64(500*clock.NsPerMs + 2_500_000), Label: "TIT2"},
		{ID: "emsg-1", TrackID: 32, Kind: events.KindEMSG, TimeBaseDen: ns, StartTicks: int64(2400 * clock.NsPerMs), EndTicks: int64(2401 * clock.NsPerMs), Label: "cue-point"},
		{ID: "sei-1", TrackID: 1, Kind: events.KindSEI, TimeBaseDen: ns, StartTicks: int64(2800 * clock.NsPerMs), EndTicks: int64(2801 * clock.NsPerMs), Payload: []byte("SEI timing metadata")},
	}
	if damaged {
		evs = append(evs,
			events.RawEvent{ID: "bad-sub", GroupID: "bad-cross", TrackID: 30, Kind: events.KindSubtitle, TimeBaseDen: ns, StartTicks: int64(1500 * clock.NsPerMs), EndTicks: int64(1700 * clock.NsPerMs), Label: "drifted caption"},
			events.RawEvent{ID: "bad-emsg", GroupID: "bad-cross", TrackID: 32, Kind: events.KindEMSG, TimeBaseDen: ns, StartTicks: int64(1580 * clock.NsPerMs), EndTicks: int64(1581 * clock.NsPerMs), Label: "drifted emsg"},
		)
	}
	return evs
}

// Generate builds the complete deterministic fixture bundle.
func Generate(req Request) (*Bundle, error) {
	damaged := req.Case == "damaged"
	d, s, db, sb, err := videoTracks(req.Seed, damaged)
	if err != nil {
		return nil, err
	}
	b := &Bundle{
		Director: d, Stage: s, DirectorBytes: db, StageBytes: sb,
		Audio48Stereo: pcmTone(OutputRate, 2, audio.LayoutStereo, req.Seed, true),
		Audio44Mono:   pcmTone(44100, 1, audio.LayoutMono, req.Seed+1, false),
		Audio51:       pcmTone(OutputRate, 6, audio.Layout51, req.Seed+2, false),
		RawEvents:     eventFixture(damaged),
		RTPPackets:    rtpFixture(),
	}
	b.RequestHash = HashBytes([]byte(fmt.Sprintf("%s|%d|v1", req.Case, req.Seed)))
	return b, nil
}
