package render

import (
	"bytes"
	"fmt"
)

// buildManifest emits a stable JSON-lines manifest.
func (s *Sequence) buildManifest() ([]byte, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `{"width":%d,"height":%d,"frames":%d}`+"\n", s.Width, s.Height, len(s.Frames))
	for _, f := range s.Frames {
		evs, err := marshalStrings(f.Events)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, `{"index":%d,"track":%d,"start":%d,"kind":"%s","label":"%s","sha256":"%s","events":%s}`+"\n",
			f.Index, f.TrackID, int64(f.Start), f.Kind, f.Label, f.SHA256, evs)
	}
	return buf.Bytes(), nil
}

func marshalStrings(values []string) (string, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('"')
		for _, r := range v {
			switch r {
			case '"', '\\':
				buf.WriteByte('\\')
				buf.WriteRune(r)
			case '\n':
				buf.WriteString(`\n`)
			default:
				if r < 0x20 {
					fmt.Fprintf(&buf, `\u%04x`, r)
				} else {
					buf.WriteRune(r)
				}
			}
		}
		buf.WriteByte('"')
	}
	buf.WriteByte(']')
	return buf.String(), nil
}
