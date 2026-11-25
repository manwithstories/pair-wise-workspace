package manifest

import (
	"bytes"
	"encoding/xml"
	"io"

	"fieldpost/internal/core"
)

// ParseDASH parses a static MPD and expands SegmentTemplate/SegmentList entries.
func ParseDASH(source string, data []byte) (*Manifest, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	m := &Manifest{Kind: KindDASH, Source: source, DASH: &DASHInfo{}}
	var rep *DASHRepresentation

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, core.ManifestError(source, dec.InputOffset(), "invalid XML: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			off := dec.InputOffset()
			switch t.Name.Local {
			case "MPD":
				if v := xmlAttr(t.Attr, "mediaPresentationDuration"); v != "" {
					d, perr := parseISODuration(v)
					if perr != nil || d < 0 {
						return nil, core.ManifestError(source, off, "invalid mediaPresentationDuration %q", v)
					}
					m.DASH.MediaPresentationDuration = d
				}
				if v := xmlAttr(t.Attr, "minBufferTime"); v != "" {
					d, perr := parseISODuration(v)
					if perr != nil || d < 0 {
						return nil, core.ManifestError(source, off, "invalid minBufferTime %q", v)
					}
					m.DASH.MinBufferTime = d
				}
			case "Representation":
				rep = &DASHRepresentation{
					ID: xmlAttr(t.Attr, "id"), Codecs: xmlAttr(t.Attr, "codecs"),
					MimeType: xmlAttr(t.Attr, "mimeType"), Offset: off,
				}
				rep.Bandwidth = atoiOrZero(xmlAttr(t.Attr, "bandwidth"))
			case "SegmentTemplate":
				if rep == nil {
					return nil, core.ManifestError(source, off, "SegmentTemplate outside Representation")
				}
				tmpl, terr := parseTemplate(dec, source, t, off)
				if terr != nil {
					return nil, terr
				}
				rep.Template = tmpl
				segs, eerr := expandTemplate(source, tmpl, off)
				if eerr != nil {
					return nil, eerr
				}
				rep.Segments = segs
			case "SegmentList":
				ts, dur, lerr := parseListAttrs(t)
				if lerr != nil {
					return nil, core.ManifestError(source, off, "%v", lerr)
				}
				segs, serr := parseSegmentURLs(dec, source, "SegmentList", ts, dur)
				if serr != nil {
					return nil, serr
				}
				if rep == nil {
					if err := rejectDuplicateNumbers(source, numsOf(segs), offsetsOf(segs)); err != nil {
						return nil, err
					}
					m.Segments = append(m.Segments, segs...)
				} else {
					rep.Segments = append(rep.Segments, segs...)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Representation" && rep != nil {
				if len(rep.Segments) == 0 {
					return nil, core.ManifestError(source, rep.Offset, "representation %q has no segments", rep.ID)
				}
				if err := rejectDuplicateNumbers(source, numsOf(rep.Segments), offsetsOf(rep.Segments)); err != nil {
					return nil, err
				}
				m.DASH.Representations = append(m.DASH.Representations, *rep)
				if len(m.Segments) == 0 {
					m.Segments = append([]Segment(nil), rep.Segments...)
				}
				rep = nil
			}
		}
	}
	if len(m.DASH.Representations) == 0 && len(m.Segments) == 0 {
		return nil, core.ManifestError(source, 0, "MPD contains no representations or segment lists")
	}
	return m, nil
}

func numsOf(segs []Segment) []int64 {
	out := make([]int64, len(segs))
	for i := range segs {
		out[i] = segs[i].Number
	}
	return out
}

func offsetsOf(segs []Segment) []int64 {
	out := make([]int64, len(segs))
	for i := range segs {
		out[i] = segs[i].Offset
	}
	return out
}

func atoiOrZero(s string) int {
	n := 0
	sign := 1
	for _, c := range s {
		if c == '-' {
			sign = -1
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return sign * n
}
