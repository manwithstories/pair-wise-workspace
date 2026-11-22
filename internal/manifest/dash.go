package manifest

import (
	"encoding/xml"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediaroute/internal/clock"
)

// Limits for DASH inputs.
const (
	MaxTracks = 64
)

// Track describes one DASH representation's essential metadata.
type Track struct {
	ID             string
	Kind           string // "video" or "audio"
	Bandwidth      int
	AudioRate      int // samples per second, audio only
	AudioChannels  int
	ColorPrimaries string // container-level colour metadata, may be ""
}

// MPD is a parsed DASH media presentation description.
type MPD struct {
	SegmentDurationTicks uint64
	SegmentCount         int
	Tracks               []Track
}

type mpdXML struct {
	Periods []struct {
		Sets []struct {
			ContentType string `xml:"contentType,attr"`
			Reps        []struct {
				ID        string `xml:"id,attr"`
				Bandwidth int    `xml:"bandwidth,attr"`
				Rate      int    `xml:"audioSamplingRate,attr"`
				Channels  int    `xml:"audioChannels,attr"`
				Color     string `xml:"colorPrimaries,attr"`
				SegList   struct {
					Duration  int  `xml:"duration,attr"` // 90 kHz ticks
					Timescale int  `xml:"timescale,attr"`
					Count     int  `xml:"count,attr"`
				} `xml:"SegmentList"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// ParseDASH parses a simplified MPD document.
func ParseDASH(data []byte) (*MPD, error) {
	var doc mpdXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("manifest: dash: %w", err)
	}
	out := &MPD{}
	for _, p := range doc.Periods {
		for _, as := range p.Sets {
			for _, r := range as.Reps {
				if len(out.Tracks) >= MaxTracks {
					return nil, fmt.Errorf("manifest: dash: exceeds %d tracks", MaxTracks)
				}
				kind := as.ContentType
				if kind != "video" && kind != "audio" {
					return nil, fmt.Errorf("manifest: dash: track %q: bad kind %q", r.ID, kind)
				}
				if r.SegList.Timescale != 0 && r.SegList.Timescale != clock.VideoClockHz {
					return nil, fmt.Errorf("manifest: dash: track %q: unsupported timescale %d", r.ID, r.SegList.Timescale)
				}
				out.Tracks = append(out.Tracks, Track{
					ID: r.ID, Kind: kind, Bandwidth: r.Bandwidth,
					AudioRate: r.Rate, AudioChannels: r.Channels, ColorPrimaries: r.Color,
				})
				if r.SegList.Duration > 0 {
					out.SegmentDurationTicks = uint64(r.SegList.Duration)
					out.SegmentCount = r.SegList.Count
				}
			}
		}
	}
	if len(out.Tracks) == 0 {
		return nil, fmt.Errorf("manifest: dash: no representations")
	}
	return out, nil
}

// ProbeTimescale reads the mdhd timescale from an init segment using go-mp4.
func ProbeTimescale(r io.ReadSeeker) (uint32, error) {
	var ts uint32
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia():
			return h.Expand()
		case mp4.BoxTypeMdhd():
			payload, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			ts = payload.(*mp4.Mdhd).Timescale
		}
		return nil, nil
	})
	if err != nil {
		return 0, fmt.Errorf("manifest: probe init segment: %w", err)
	}
	if ts == 0 {
		return 0, fmt.Errorf("manifest: probe init segment: no mdhd box")
	}
	return ts, nil
}
