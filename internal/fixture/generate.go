package fixture

import (
	"fmt"
	"math/rand"
	"strings"

	"mediascan/internal/clock"
	"mediascan/internal/manifest"
	"mediascan/internal/probe"
	"mediascan/internal/repair"
)

// Set is the complete generated input for one run.
type Set struct {
	Seed            int64
	DropEvery       int
	Damaged         bool
	SourceBytes     []byte
	MP4             []byte
	Manifest        []byte
	HLSVariants     map[string][]byte
	Loader          manifest.Loader
	RTP             [][]byte
	Timestamps      []clock.Observed
	Representations []probe.Representation
	Latencies       []int64
	AVOffsetNs      int64
	Warnings        []string
}

// SegmentSeconds is the fixed segment duration of the generated programme.
const SegmentSeconds = 2

// SegmentCount is the fixed programme length in segments (60 s total).
const SegmentCount = 30

// Options configures generation.
type Options struct {
	Seed      int64
	DropEvery int
	Damaged   bool
	// TruncateTail removes this many bytes from the end of the container,
	// producing the truncated-index sample used to prove failure isolation.
	TruncateTail int
	// ConflictingAudioMask sets an audio channel mask whose popcount disagrees
	// with the sample entry, exercising conflict normalisation.
	ConflictingAudioMask bool
	// ConflictingColor gives the second video rendition a different colour
	// description, exercising cross-representation reconciliation.
	ConflictingColor bool
}

// Generate produces the full fixture set deterministically from the seed.
func Generate(opt Options) *Set {
	if opt.Seed == 0 {
		opt.Seed = 1
	}
	rng := rand.New(rand.NewSource(opt.Seed))
	set := &Set{Seed: opt.Seed, DropEvery: opt.DropEvery, Damaged: opt.Damaged, HLSVariants: map[string][]byte{}}

	video := []MP4Track{
		{ID: 1, Handler: "vide", Timescale: 90000, Width: 1280, Height: 720,
			Primaries: 1, Transfer: 1, Matrix: 1},
		{ID: 3, Handler: "vide", Timescale: 90000, Width: 1920, Height: 1080,
			Primaries: 9, Transfer: 16, Matrix: 9},
	}
	if !opt.ConflictingColor {
		// Renditions agree by default; the conflict option keeps BT.2020 on the
		// second rendition while BT.709 stays on the first.
		video[1].Primaries, video[1].Transfer, video[1].Matrix = 1, 1, 1
	}
	audio := MP4Track{ID: 2, Handler: "soun", Timescale: 48000, Channels: 6, SampleRate: 48000,
		ChanConfig: 6, ObjectType: 0x40}
	if opt.Damaged {
		audio.Channels = 2 // conflicts with the 5.1 mask below
	}

	// Audio media time starts later than video by a fixed amount, which the
	// prepared output must compensate for. Ten to twelve milliseconds is the
	// realistic drift range this fixture models.
	avOffsetNs := int64(10+rng.Intn(3)) * 1_000_000
	audioStartTicks := uint64(avOffsetNs * 48000 / clock.NanosPerSecond)

	var frags []MP4Fragment
	var obs []clock.Observed
	for i := 0; i < SegmentCount; i++ {
		for _, tr := range video {
			dur := uint32(90000 * SegmentSeconds)
			frags = append(frags, MP4Fragment{TrackID: tr.ID, BaseDecodeTime: uint64(i) * uint64(dur),
				Samples: []MP4Sample{{Duration: dur, Data: SampleData(tr.ID, i, 0), Sync: true}}})
		}
		frags = append(frags, MP4Fragment{TrackID: audio.ID, BaseDecodeTime: uint64(i)*48000*SegmentSeconds + audioStartTicks,
			Samples: []MP4Sample{{Duration: 48000 * SegmentSeconds, Data: SampleData(audio.ID, i, 0), Sync: true}}})

		start := uint64(i * 90000 * SegmentSeconds)
		valid := true
		hasStart := true
		switch {
		case opt.Damaged && i%7 == 3:
			valid = false
		case opt.Damaged && i%11 == 5:
			hasStart = false
		}
		obs = append(obs, clock.Observed{
			ID: segName(i), Start: start, HasStart: hasStart,
			Duration: int64(SegmentSeconds) * clock.NanosPerSecond, Valid: valid,
		})
	}
	// The damaged set starts its counter close to the 33-bit bound so the
	// programme crosses the wraparound halfway through.
	if opt.Damaged {
		const wrapAt = SegmentCount / 2
		offset := uint64(clock.WrapModulus) - uint64(wrapAt*90000*SegmentSeconds)
		for i := range obs {
			if obs[i].HasStart {
				obs[i].Start = (obs[i].Start + offset) % uint64(clock.WrapModulus)
			}
		}
	}

	set.MP4 = BuildFragmentedMP4(append(video, audio), frags)
	if opt.TruncateTail > 0 {
		// Chop the tail so the final fragment's sample runs past EOF.
		trim := opt.TruncateTail
		if trim >= len(set.MP4) {
			trim = len(set.MP4) / 2
		}
		set.MP4 = append([]byte(nil), set.MP4[:len(set.MP4)-trim]...)
	}
	set.Timestamps = obs
	set.SourceBytes = set.MP4

	// Two HLS renditions and a DASH manifest over the same timeline.
	set.Manifest, set.HLSVariants = buildManifests(set)

	set.Representations = buildRepresentations(video, audio, opt)
	set.AVOffsetNs = avOffsetNs
	set.RTP = GenerateRTP(RTPOptions{
		Packets:   SegmentCount * 40,
		DropEvery: opt.DropEvery,
		Seed:      opt.Seed,
	})
	if opt.Damaged {
		// Corrupt one datagram in the damaged set.
		if len(set.RTP) > 5 {
			set.RTP[5] = []byte{0x80, 0x00}
		}
	}
	return set
}

func segName(i int) string {
	return fmt.Sprintf("seg-%02d", i)
}

func buildRepresentations(video []MP4Track, audio MP4Track, opt Options) []probe.Representation {
	mask := uint64(0x3F) // FL FR FC LFE BL BR
	if opt.ConflictingAudioMask {
		mask = 0x3 // FL FR while the entry declares six channels
	}
	reps := []probe.Representation{
		{ID: "video-720", Video: &probe.VideoInfo{TrackID: video[0].ID, Width: 1280, Height: 720,
			Color: probe.ColorInfo{Present: true, Primaries: video[0].Primaries,
				Transfer: video[0].Transfer, Matrix: video[0].Matrix, Complete: true}}},
		{ID: "video-1080", Video: &probe.VideoInfo{TrackID: video[1].ID, Width: 1920, Height: 1080,
			Color: probe.ColorInfo{Present: true, Primaries: video[1].Primaries,
				Transfer: video[1].Transfer, Matrix: video[1].Matrix, Complete: true}}},
		{ID: "audio-51", Audio: &probe.AudioInfo{TrackID: audio.ID, EntryChannels: int(audio.Channels),
			Mask: mask, HasMask: true, LayoutCount: 6, SampleRate: int(audio.SampleRate),
			Timescale: int(audio.Timescale), ObjectType: audio.ObjectType, HasObjectType: true}},
	}
	return reps
}

func buildManifests(set *Set) ([]byte, map[string][]byte) {
	var media strings.Builder
	media.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	media.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", SegmentSeconds))
	media.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-MAP:URI=\"init.mp4\"\n")
	for i := 0; i < SegmentCount; i++ {
		if set.Damaged && i%13 == 7 {
			media.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&media, "#EXTINF:%d.000,\n%s.m4s\n", SegmentSeconds, segName(i))
	}
	media.WriteString("#EXT-X-ENDLIST\n")
	body := []byte(media.String())
	set.HLSVariants["video_800.m3u8"] = body
	set.HLSVariants["video_1600.m3u8"] = body

	master := []byte(`#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.64001f,mp4a.40.2"
video_800.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1600000,CODECS="avc1.640028,mp4a.40.2"
video_1600.m3u8
`)
	set.Loader = func(uri string) ([]byte, error) {
		b, ok := set.HLSVariants[uri]
		if !ok {
			return nil, fmt.Errorf("missing variant %q", uri)
		}
		return b, nil
	}
	return master, set.HLSVariants
}

var _ = repair.Analyze
