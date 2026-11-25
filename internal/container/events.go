package container

import "bytes"

// ExtractSEI returns H.264 Supplemental Enhancement Information NAL payloads
// from Annex-B sample data. It does not decode video; it only locates NAL type
// 6 and preserves its bytes for attribution/hashing.
func ExtractSEI(sample []byte) [][]byte {
	var out [][]byte
	for start := 0; start < len(sample)-4; {
		sc, n := nextStartCode(sample, start)
		if sc < 0 || sc+n >= len(sample) {
			break
		}
		nalStart := sc + n
		next, nextN := nextStartCode(sample, nalStart)
		nalEnd := len(sample)
		if next >= 0 {
			nalEnd = next
		}
		if nalStart < len(sample) {
			nalType := sample[nalStart] & 0x1f
			if nalType == 6 && nalEnd > nalStart {
				out = append(out, append([]byte(nil), sample[nalStart:nalEnd]...))
			}
		}
		if next < 0 {
			break
		}
		start = next + nextN
	}
	return out
}

func nextStartCode(b []byte, from int) (offset, size int) {
	for i := from; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i, 3
		}
		if i+3 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return i, 4
		}
	}
	return -1, 0
}

// EventsFromTrack maps sample-carried subtitles, chapters, ID3 and SEI to
// cross-track timed events. Each event retains its owning track and sample.
func EventsFromTrack(t Track) []Event {
	events := make([]Event, 0)
	for _, s := range t.Samples {
		switch t.Kind {
		case KindSubtitle:
			payload := append([]byte(nil), s.Data...)
			events = append(events, Event{
				Kind: EventSubtitle, TrackID: t.ID, SampleIndex: s.Index,
				RawTime: s.PTS, Timescale: t.Timescale, TimeNS: s.TimeNS,
				Label: string(bytes.TrimSpace(payload)), Payload: payload, Hash: HashBytes(payload),
			})
		case KindChapters:
			payload := append([]byte(nil), s.Data...)
			events = append(events, Event{
				Kind: EventChapter, TrackID: t.ID, SampleIndex: s.Index,
				RawTime: s.PTS, Timescale: t.Timescale, TimeNS: s.TimeNS,
				Label: string(bytes.TrimSpace(payload)), Payload: payload, Hash: HashBytes(payload),
			})
		case KindMetadata:
			if bytes.HasPrefix(s.Data, []byte("ID3")) {
				payload := append([]byte(nil), s.Data...)
				events = append(events, Event{
					Kind: EventID3, TrackID: t.ID, SampleIndex: s.Index,
					RawTime: s.PTS, Timescale: t.Timescale, TimeNS: s.TimeNS,
					Label: "ID3v2", Payload: payload, Hash: HashBytes(payload),
				})
			}
		case KindVideo:
			for _, sei := range ExtractSEI(s.Data) {
				events = append(events, Event{
					Kind: EventSEI, TrackID: t.ID, SampleIndex: s.Index,
					RawTime: s.PTS, Timescale: t.Timescale, TimeNS: s.TimeNS,
					Label: "H.264 SEI NAL", Payload: sei, Hash: HashBytes(sei),
				})
			}
		}
	}
	return events
}

// CollectEvents builds the deterministic event list for an asset.
func CollectEvents(a *Asset) []Event {
	events := make([]Event, 0, len(a.Events))
	events = append(events, a.Events...)
	for _, t := range a.Tracks {
		events = append(events, EventsFromTrack(t)...)
	}
	sortEvents(events)
	return events
}

func sortEvents(events []Event) {
	// Insertion sort keeps this helper dependency-free and stable for tests.
	for i := 1; i < len(events); i++ {
		for j := i; j > 0; j-- {
			if eventLess(events[j], events[j-1]) {
				events[j-1], events[j] = events[j], events[j-1]
			} else {
				break
			}
		}
	}
}

func eventLess(a, b Event) bool {
	if a.TimeNS != b.TimeNS {
		return a.TimeNS < b.TimeNS
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.TrackID != b.TrackID {
		return a.TrackID < b.TrackID
	}
	return a.SampleIndex < b.SampleIndex
}
