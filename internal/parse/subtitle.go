package parse

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// SubtitleCue is one subtitle display interval.
type SubtitleCue struct {
	ID    string     `json:"id"`
	Start clock.Tick `json:"start"`
	Dur   clock.Tick `json:"dur"`
	Text  string     `json:"text"`
}

// Chapter is one chapter marker.
type Chapter struct {
	ID    string     `json:"id"`
	Start clock.Tick `json:"start"`
	Title string     `json:"title"`
}

// EmsgEvent is an ISO BMFF in-band event message.
type EmsgEvent struct {
	ID        string     `json:"id"`
	Scheme    string     `json:"scheme"`
	Value     string     `json:"value"`
	Start     clock.Tick `json:"start"`
	Duration  clock.Tick `json:"dur"`
	Message   []byte     `json:"message"`
	Timescale uint32     `json:"timescale"`
}

// SEIEvent is one unregistered user-data SEI message.
type SEIEvent struct {
	ID      string     `json:"id"`
	UUID    [16]byte   `json:"uuid"`
	Start   clock.Tick `json:"start"`
	Payload []byte     `json:"payload"`
}

// SubtitleXML is the stpp sample payload this pipeline writes.
type SubtitleXML struct {
	XMLName xml.Name `xml:"tt"`
	Cues    []CueXML `xml:"p"`
}

// CueXML uses integer ticks so no float rounding can perturb a rerun.
type CueXML struct {
	ID    string `xml:"id,attr,omitempty"`
	Begin int64  `xml:"begin,attr"`
	Dur   int64  `xml:"dur,attr"`
	Text  string `xml:",chardata"`
}

// ParseSubtitles decodes a stpp sample. Ticks are absolute unified ticks;
// the sample timescale is accepted for validation only because the payload
// is already expressed on the unified base.
func ParseSubtitles(data []byte) ([]SubtitleCue, error) {
	var doc SubtitleXML
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: subtitle xml: %v", model.ErrBadLength, err))
	}
	if len(doc.Cues) > MaxCues {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: %d cues", model.ErrLimit, len(doc.Cues)))
	}
	out := make([]SubtitleCue, 0, len(doc.Cues))
	for i, c := range doc.Cues {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("cue-%d", i+1)
		}
		if err := clock.SoundCheck(clock.Tick(c.Begin), clock.Tick(c.Dur)); err != nil {
			return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: cue %s: %v", model.ErrBadLength, id, err))
		}
		out = append(out, SubtitleCue{ID: id, Start: clock.Tick(c.Begin), Dur: clock.Tick(c.Dur), Text: c.Text})
	}
	return out, nil
}

// EncodeSubtitles writes a stpp sample deterministically.
func EncodeSubtitles(cues []SubtitleCue) ([]byte, error) {
	if len(cues) > MaxCues {
		return nil, fmt.Errorf("%w: %d cues", model.ErrLimit, len(cues))
	}
	doc := SubtitleXML{XMLName: xml.Name{Local: "tt"}}
	for _, c := range cues {
		if err := clock.SoundCheck(c.Start, c.Dur); err != nil {
			return nil, fmt.Errorf("%w: cue %s: %v", model.ErrInvalid, c.ID, err)
		}
		doc.Cues = append(doc.Cues, CueXML{ID: c.ID, Begin: int64(c.Start), Dur: int64(c.Dur), Text: c.Text})
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}
