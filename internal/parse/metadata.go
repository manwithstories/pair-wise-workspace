package parse

import (
	"fmt"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// GlobalTrackID labels events that are not owned by a media track
// (chapters and in-band event messages).
const GlobalTrackID uint32 = 0xFFFF

// TimedID3 is an ID3 tag carried in one media sample.
type TimedID3 struct {
	TrackID uint32
	Start   clock.Tick
	Tag     ID3Tag
}

// TimedSEI is an SEI message carried in one video sample.
type TimedSEI struct {
	TrackID uint32
	Start   clock.Tick
	Event   SEIEvent
}

// TimedCue is a subtitle cue carried in one text sample.
type TimedCue struct {
	TrackID uint32
	Cue     SubtitleCue
}

// Metadata is every timed annotation found in a movie.
type Metadata struct {
	Subtitles []TimedCue  `json:"subtitles"`
	Chapters  []Chapter   `json:"chapters"`
	ID3       []TimedID3  `json:"id3"`
	Emsg      []EmsgEvent `json:"emsg"`
	SEI       []TimedSEI  `json:"sei"`
}

// ExtractMetadata walks tracks and top-level boxes for annotations. Unknown
// boxes are ignored here; they stay available in Movie.Extras/Udta for
// preservation.
func ExtractMetadata(m *Movie) (*Metadata, error) {
	meta := &Metadata{}
	for _, t := range m.Tracks {
		for _, s := range t.Samples {
			start, err := clock.ScaleToTicks(int64(s.DTS), t.Timescale)
			if err != nil {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: track %d sample %d time: %v", model.ErrBadLength, t.ID, s.Index, err))
			}
			switch t.Handler {
			case "text", "sbtl":
				cues, err := ParseSubtitles(s.Data)
				if err != nil {
					return nil, err
				}
				for _, c := range cues {
					meta.Subtitles = append(meta.Subtitles, TimedCue{TrackID: t.ID, Cue: c})
				}
			case "soun":
				if len(s.Data) >= 3 && string(s.Data[0:3]) == "ID3" {
					tag, err := ParseID3(s.Data)
					if err != nil {
						return nil, err
					}
					meta.ID3 = append(meta.ID3, TimedID3{TrackID: t.ID, Start: start, Tag: *tag})
				}
			case "vide":
				events, err := ParseSEI(s.Data)
				if err != nil {
					return nil, err
				}
				for i := range events {
					events[i].Start = start
					meta.SEI = append(meta.SEI, TimedSEI{TrackID: t.ID, Start: start, Event: events[i]})
				}
			}
		}
	}
	// Chapters live in moov/udta/chpl.
	for i := range m.Udta {
		if m.Udta[i].String() == "chpl" {
			chapters, err := parseChpl(&m.Udta[i])
			if err != nil {
				return nil, err
			}
			meta.Chapters = chapters
			break
		}
	}
	// emsg boxes may appear at top level or inside moof.
	for i := range m.Extras {
		if m.Extras[i].String() != "emsg" {
			continue
		}
		ev, err := ParseEmsg(&m.Extras[i])
		if err != nil {
			return nil, err
		}
		meta.Emsg = append(meta.Emsg, *ev)
	}
	return meta, nil
}

// Events flattens metadata into the shared event model. The payload bytes
// are exactly the bytes stored in the container, so a re-parse reproduces
// the same digest.
func (md *Metadata) Events() []model.Event {
	var out []model.Event
	for _, c := range md.Subtitles {
		out = append(out, model.Event{
			Kind:      model.KindSubtitle,
			TrackID:   c.TrackID,
			Start:     c.Cue.Start,
			Duration:  c.Cue.Dur,
			PayloadID: c.Cue.ID,
			Payload:   []byte(c.Cue.Text),
			Sequence:  0,
		})
	}
	for i, ch := range md.Chapters {
		out = append(out, model.Event{
			Kind:      model.KindChapter,
			TrackID:   GlobalTrackID,
			Start:     ch.Start,
			PayloadID: ch.ID,
			Payload:   []byte(ch.Title),
			Sequence:  uint32(i + 1),
		})
	}
	for _, t := range md.ID3 {
		out = append(out, model.Event{
			Kind:      model.KindID3,
			TrackID:   t.TrackID,
			Start:     t.Start,
			PayloadID: t.Tag.ID,
			Payload:   []byte(t.Tag.Text),
		})
	}
	for _, e := range md.Emsg {
		out = append(out, model.Event{
			Kind:      model.KindEmsg,
			TrackID:   GlobalTrackID,
			Start:     e.Start,
			Duration:  e.Duration,
			PayloadID: e.ID,
			Payload:   append([]byte(nil), e.Message...),
		})
	}
	for _, s := range md.SEI {
		out = append(out, model.Event{
			Kind:      model.KindSEI,
			TrackID:   s.TrackID,
			Start:     s.Start,
			PayloadID: s.Event.ID,
			Payload:   append([]byte(nil), s.Event.Payload...),
		})
	}
	return out
}

// FrameEvents returns one event per media sample of the given tracks,
// used for the offscreen review sequence.
func FrameEvents(m *Movie) ([]model.Event, error) {
	var out []model.Event
	for _, t := range m.Tracks {
		if t.Handler != "vide" {
			continue
		}
		for _, s := range t.Samples {
			start, err := clock.ScaleToTicks(int64(s.DTS), t.Timescale)
			if err != nil {
				return nil, err
			}
			dur, err := clock.ScaleToTicks(int64(s.Duration), t.Timescale)
			if err != nil {
				return nil, err
			}
			out = append(out, model.Event{
				Kind:      model.KindFrame,
				TrackID:   t.ID,
				TrackTime: uint32(s.DTS),
				Start:     start,
				Duration:  dur,
				Sequence:  uint32(s.Index),
				Keyframe:  s.Sync,
			})
		}
	}
	return out, nil
}
