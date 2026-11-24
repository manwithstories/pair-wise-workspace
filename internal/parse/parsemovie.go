package parse

import (
	"bytes"
	"fmt"

	"mediarun/internal/model"
)

// ParseMovie decodes data into a Movie. Malformed lengths and truncation
// are reported as corrupt-input errors.
func ParseMovie(data []byte) (*Movie, error) {
	boxes, err := Scan(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	m := &Movie{}
	moov := FindTop(boxes, "moov")
	if moov == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: moov missing", model.ErrTruncated))
	}
	m.HasMoov = true
	mdat := FindTop(boxes, "mdat")
	if mdat == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: mdat missing", model.ErrTruncated))
	}
	m.HasMdat = true
	mdatStart := mdat.Offset + int64(mdat.HeaderSize)

	mvhd, err := parseMvhd(moov.Children)
	if err != nil {
		return nil, err
	}
	m.Timescale = mvhd.Timescale
	m.Duration = mvhdDuration(mvhd.DurationV0, mvhd.DurationV1, mvhd.GetVersion())

	for _, child := range moov.Children {
		switch child.String() {
		case "trak":
			track, err := parseTrak(child, data, mdatStart)
			if err != nil {
				return nil, err
			}
			m.Tracks = append(m.Tracks, *track)
		case "udta":
			// Every udta child is preserved verbatim, known or not, so a
			// rewrite keeps chapter and metadata boxes untouched.
			m.Udta = append(m.Udta, child.Children...)
		case "mvhd":
			// already handled
		}
	}
	for _, b := range boxes {
		switch b.String() {
		case "ftyp", "moov", "mdat", "udta":
		default:
			m.Extras = append(m.Extras, b)
		}
	}
	return m, nil
}
