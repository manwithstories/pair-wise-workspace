package testdata

import (
	"fmt"
	"math/rand"
	"strings"

	"archive/internal/mp4"
)

// segTracks describes the track layout of a generated asset.
type segTracks struct {
	mux    []mp4.MuxTrack
	video  int // index of video track in mux
	audio  int
	sub    int // -1 when absent
	audio2 int // second audio (docu-dash), -1 when absent
}

func stdTracks() segTracks {
	return segTracks{
		mux: []mp4.MuxTrack{
			{ID: 1, Type: "video", Codec: "avc1", Timescale: 90000, Language: "eng", Width: 640, Height: 360},
			{ID: 2, Type: "audio", Codec: "mp4a", Timescale: 48000, Language: "jpn"},
			{ID: 3, Type: "subtitle", Codec: "wvtt", Timescale: 1000},
		},
		video: 0, audio: 1, sub: 2, audio2: -1,
	}
}

// buildSegment muxes one fMP4 segment. bases[i] is the DTS base of track i.
func buildSegment(rng *rand.Rand, st segTracks, bases []int64, tfdt32 bool, videoPayload int) []byte {
	ws := &mp4.MemWS{}
	m, err := mp4.NewMuxer(ws, st.mux)
	if err != nil {
		panic(err)
	}
	m.Tfdt32 = tfdt32
	frags := make([]mp4.FragTrack, len(st.mux))
	for ti := range st.mux {
		var samples []mp4.Sample
		switch ti {
		case st.video:
			for i := 0; i < 30; i++ {
				payload := make([]byte, videoPayload/30+rng.Intn(90))
				rng.Read(payload)
				samples = append(samples, mp4.Sample{
					DTS: bases[ti] + int64(i)*3000, Duration: 3000,
					Keyframe: i == 0, Data: payload,
				})
			}
		case st.audio, st.audio2:
			for i := 0; i < 47; i++ {
				payload := make([]byte, 200+rng.Intn(200))
				rng.Read(payload)
				samples = append(samples, mp4.Sample{
					DTS: bases[ti] + int64(i)*1024, Duration: 1024, Data: payload,
				})
			}
		case st.sub:
			samples = append(samples, mp4.Sample{
				DTS: bases[ti], Duration: 1000,
				Data: []byte(fmt.Sprintf("WEBVTT cue at %d", bases[ti])),
			})
		}
		frags[ti] = mp4.FragTrack{Samples: samples}
	}
	if err := m.AddFragment(frags); err != nil {
		panic(err)
	}
	return ws.Bytes()
}

// audioBase returns the continuous audio DTS base for segment i (47 AAC
// frames of 1024 ticks per 1-second segment, 512-tick priming offset).
func audioBase(i int) int64 { return 512 + int64(i)*47*1024 }

// genHLSAsset writes a 4-segment HLS asset (master+media playlist) plus
// migration artifacts: a stale duplicate and a conflicting copy.
func genHLSAsset(rng *rand.Rand, dir string, wrap bool) (string, error) {
	st := stdTracks()
	tfdt32 := wrap
	var entries []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("seg_%05d.m4s", i+1)
		var bases []int64
		if wrap {
			bases = []int64{
				(0xFFFFE000 + int64(i)*90000) & 0xFFFFFFFF,
				(0xFFFF4480 + int64(i)*48128) & 0xFFFFFFFF,
				(0xFFFFFC18 + int64(i)*1000) & 0xFFFFFFFF,
			}
		} else {
			bases = []int64{int64(i) * 90000, audioBase(i), int64(i) * 1000}
		}
		data := buildSegment(rng, st, bases, tfdt32, 36000)
		if err := writeFile(dir, name, data); err != nil {
			return "", err
		}
		entries = append(entries, name)
	}
	if !wrap {
		// migration artifacts: identical duplicate and conflicting copy
		dup, _ := readFile(dir, entries[1])
		if err := writeFile(dir, "seg_00002.dup.m4s", dup); err != nil {
			return "", err
		}
		conflict := buildSegment(rng, st, []int64{2 * 90000, audioBase(2), 2000}, false, 36000)
		if err := writeFile(dir, "seg_00003.conflict.m4s", conflict); err != nil {
			return "", err
		}
	}
	media := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n"
	for _, e := range entries {
		media += "#EXTINF:1.0,\n" + e + "\n"
	}
	media += "#EXT-X-ENDLIST\n"
	if err := writeFile(dir, "media.m3u8", []byte(media)); err != nil {
		return "", err
	}
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000\nmedia.m3u8\n"
	return "master.m3u8", writeFile(dir, "master.m3u8", []byte(master))
}

// genDASHAsset writes a 3-segment DASH asset (SegmentList) with a second,
// untagged audio track to exercise source-ID fallback mapping.
func genDASHAsset(rng *rand.Rand, dir string) (string, error) {
	st := stdTracks()
	st.mux[1].Language = "eng"
	st.mux = append(st.mux, mp4.MuxTrack{ID: 5, Type: "audio", Codec: "mp4a", Timescale: 48000})
	st.audio2 = 3
	var files []string
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("seg_%05d.m4s", i+1)
		bases := []int64{int64(i) * 90000, audioBase(i), int64(i) * 1000, audioBase(i)}
		if err := writeFile(dir, name, buildSegment(rng, st, bases, false, 30000)); err != nil {
			return "", err
		}
		files = append(files, name)
	}
	var urls strings.Builder
	for _, f := range files {
		urls.WriteString(`<SegmentURL media="` + f + `"/>`)
	}
	mpd := `<?xml version="1.0"?><MPD><Period><AdaptationSet contentType="video">` +
		`<Representation><SegmentList timescale="90000" duration="90000">` +
		urls.String() + `</SegmentList></Representation></AdaptationSet></Period></MPD>`
	return "manifest.mpd", writeFile(dir, "manifest.mpd", []byte(mpd))
}

// genBigAsset writes a 6-segment DASH asset (SegmentTemplate) with ~3 MiB
// of payload so checkpointing commits multiple chunks at 1 MiB.
func genBigAsset(rng *rand.Rand, dir string) (string, error) {
	st := segTracks{
		mux: []mp4.MuxTrack{
			{ID: 1, Type: "video", Codec: "avc1", Timescale: 90000, Language: "eng", Width: 640, Height: 360},
			{ID: 2, Type: "audio", Codec: "mp4a", Timescale: 48000, Language: "eng"},
		},
		video: 0, audio: 1, sub: -1, audio2: -1,
	}
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("seg_%05d.m4s", i+1)
		bases := []int64{int64(i) * 90000, audioBase(i)}
		if err := writeFile(dir, name, buildSegment(rng, st, bases, false, 480000)); err != nil {
			return "", err
		}
	}
	mpd := `<?xml version="1.0"?><MPD><Period><AdaptationSet>` +
		`<SegmentTemplate media="seg_$Number%05d$.m4s" timescale="90000" startNumber="1">` +
		`<SegmentTimeline><S d="90000" r="5"/></SegmentTimeline></SegmentTemplate>` +
		`</AdaptationSet></Period></MPD>`
	return "manifest.mpd", writeFile(dir, "manifest.mpd", []byte(mpd))
}
