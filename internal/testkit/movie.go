package testkit

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
)

// GenerateMedia builds a small but complete movie with subtitle, chapter,
// ID3, emsg and SEI metadata. The same seed always produces the same bytes.
func GenerateMedia(seed int64) (*Media, error) {
	rng := rand.New(rand.NewSource(seed))
	specs := []TrackSpec{
		{ID: 1, Handler: "vide", Name: "stage-main", Timescale: 90000, Samples: 18, StartTick: 0},
		{ID: 2, Handler: "vide", Name: "side-stage", Timescale: 90000, Samples: 14, StartTick: 0},
		{ID: 3, Handler: "vide", Name: "mobile", Timescale: 90000, Samples: 12, StartTick: 0},
		{ID: 4, Handler: "soun", Name: "stage-audio", Timescale: 48000, Samples: 12, StartTick: 0},
		{ID: 5, Handler: "text", Name: "subtitles", Timescale: 90000, Samples: 6, StartTick: 0},
	}
	m := &parse.Movie{Timescale: 1000}
	media := &Media{VideoTrackID: 1, AudioTrackID: 4, SubtitleTrackID: 5}

	seiUUID := [16]byte{'m', 'e', 'd', 'i', 'a', 'r', 'u', 'n', '-', 's', 'e', 'i', 0, 0, 0, seedByte(seed)}
	for _, spec := range specs {
		if spec.Samples > MaxVideoSamples {
			return nil, fmt.Errorf("%w: %d samples", model.ErrLimit, spec.Samples)
		}
		track := parse.Track{
			ID:        spec.ID,
			Handler:   spec.Handler,
			Timescale: spec.Timescale,
			Language:  "und",
		}
		switch spec.Handler {
		case "vide":
			track.Width, track.Height = 64, 36
			track.Entry = entryBox("avc1", avc1Body(track.Width, track.Height))
		case "soun":
			track.Entry = entryBox("mp4a", mp4aBody(spec.Timescale))
		default:
			track.Entry = entryBox("stpp", []byte{0, 0, 0, 0})
		}
		var dts uint64
		sampleDur := uint32(spec.Timescale) / uint32(max(1, spec.Samples/3))
		if sampleDur == 0 {
			sampleDur = 1
		}
		for i := 0; i < spec.Samples; i++ {
			var data []byte
			var sync bool
			switch spec.Handler {
			case "vide":
				sync = i%3 == 0
				text := fmt.Sprintf("frame-%02d-%04d", i, rng.Intn(1000))
				var err error
				data, err = parse.EncodeSEI(seiUUID, []byte(text))
				if err != nil {
					return nil, err
				}
				if i%4 == 0 {
					pad := make([]byte, 4+len(data))
					binary.BigEndian.PutUint32(pad[:4], uint32(len(data)))
					copy(pad[4:], data)
					data = pad
				}
			case "soun":
				sync = true
				var err error
				data, err = parse.EncodeID3(&parse.ID3Tag{
					ID:    fmt.Sprintf("audio-%02d", i),
					Text:  fmt.Sprintf("take-%d", 1+i%2),
					Owner: "stage",
					Private: []byte{
						byte(i), byte(spec.ID),
					},
				})
				if err != nil {
					return nil, err
				}
			default:
				sync = true
				var err error
				data, err = parse.EncodeSubtitles([]parse.SubtitleCue{{
					ID:    fmt.Sprintf("cue-%02d", i),
					Start: clock.Tick(i) * 90000 / clock.Tick(max(1, spec.Samples)),
					Dur:   45000,
					Text:  fmt.Sprintf("line %d", i),
				}})
				if err != nil {
					return nil, err
				}
			}
			track.Samples = append(track.Samples, parse.Sample{
				Index:    i + 1,
				DTS:      dts,
				Duration: sampleDur,
				Sync:     sync,
				Data:     data,
			})
			if sync {
				track.Keyframes = append(track.Keyframes, uint32(i+1))
			}
			dts += uint64(sampleDur)
		}
		m.Tracks = append(m.Tracks, track)
	}

	chapters := []parse.Chapter{
		{ID: "chapter-1", Start: 0, Title: "Overture"},
		{ID: "chapter-2", Start: 2 * clock.TickRate, Title: "Main set"},
		{ID: "chapter-3", Start: 4 * clock.TickRate, Title: "Encore"},
	}
	chpl, err := encodeChplForTest(chapters)
	if err != nil {
		return nil, err
	}
	m.Udta = []parse.RawBox{{
		Type:       [4]byte{'c', 'h', 'p', 'l'},
		Size:       uint64(8 + len(chpl)),
		HeaderSize: 8,
		Payload:    chpl,
	}}
	media.Chapters = chapters

	emsg, err := parse.EncodeEmsg(parse.EmsgEvent{
		ID:        "emsg-demo",
		Scheme:    "urn:mediarun:marker",
		Value:     "set-change",
		Start:     2 * clock.TickRate,
		Duration:  clock.TickRate / 2,
		Message:   []byte("camera-b"),
		Timescale: 90000,
	})
	if err != nil {
		return nil, err
	}
	m.Extras = append(m.Extras, *emsg)
	parsedEmsg, err := parse.ParseEmsg(emsg)
	if err != nil {
		return nil, err
	}
	media.Emsg = append(media.Emsg, *parsedEmsg)

	raw, err := parse.EncodeMovie(m)
	if err != nil {
		return nil, err
	}
	// Encode then parse: the expected events come from the same path the
	// verifier will use.
	reparsed, err := parse.ParseMovie(raw)
	if err != nil {
		return nil, err
	}
	meta, err := parse.ExtractMetadata(reparsed)
	if err != nil {
		return nil, err
	}
	media.Movie = reparsed
	media.Bytes = raw
	media.ExpectedEvents = meta.Events()
	return media, nil
}
