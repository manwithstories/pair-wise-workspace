package parse

import (
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/model"
)

// parseTrak decodes one trak box: header, timescale, sample tables and the
// sample bytes from mdat.
func parseTrak(trak RawBox, file []byte, mdatStart int64) (*Track, error) {
	tkhdRaw := findPath(trak.Children, "tkhd")
	if tkhdRaw == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: tkhd missing", model.ErrTruncated))
	}
	var tkhd mp4.Tkhd
	if err := unmarshal(tkhdRaw, &tkhd); err != nil {
		return nil, err
	}

	mdia := findPath(trak.Children, "mdia")
	if mdia == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: mdia missing", model.ErrTruncated))
	}
	mdhdRaw := findPath(mdia.Children, "mdhd")
	if mdhdRaw == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: mdhd missing", model.ErrTruncated))
	}
	var mdhd mp4.Mdhd
	if err := unmarshal(mdhdRaw, &mdhd); err != nil {
		return nil, err
	}
	if mdhd.Timescale == 0 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d timescale is zero", model.ErrBadLength, tkhd.TrackID))
	}
	var hdlr mp4.Hdlr
	if hdlrRaw := findPath(mdia.Children, "hdlr"); hdlrRaw != nil {
		if err := unmarshal(hdlrRaw, &hdlr); err != nil {
			return nil, err
		}
	}

	t := &Track{
		ID:        tkhd.TrackID,
		Handler:   string(hdlr.HandlerType[:]),
		Timescale: mdhd.Timescale,
		Duration:  mdhdDurationV(mdhd.DurationV0, mdhd.DurationV1, mdhd.GetVersion()),
		Language:  decodeLanguage(mdhd.Language),
	}
	if t.Handler == "vide" {
		t.Width = uint16(tkhd.Width >> 16)
		t.Height = uint16(tkhd.Height >> 16)
	}

	stbl := findPath(mdia.Children, "minf", "stbl")
	if stbl == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: stbl missing", model.ErrTruncated))
	}
	if err := t.parseStsd(stbl.Children); err != nil {
		return nil, err
	}
	return t, nil
}

func mdhdDurationV(v0 uint32, v1 uint64, version uint8) uint64 {
	if version == 0 {
		return uint64(v0)
	}
	return v1
}

// parseStsd keeps the first sample entry verbatim and validates the entry
// count.
func (t *Track) parseStsd(stbl []RawBox) error {
	raw := FindBox(stbl, "stsd")
	if raw == nil {
		return model.Classify(model.ClassCorrupt, fmt.Errorf("%w: stsd missing", model.ErrTruncated))
	}
	var stsd mp4.Stsd
	if err := unmarshal(raw, &stsd); err != nil {
		return err
	}
	// The entry count lives after the 8-byte header; children follow.
	if len(raw.Children) == 0 {
		return model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d has no sample entry", model.ErrTruncated, t.ID))
	}
	if stsd.EntryCount != uint32(len(raw.Children)) {
		return model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d stsd declares %d entries, found %d",
				model.ErrBadLength, t.ID, stsd.EntryCount, len(raw.Children)))
	}
	t.Entry = raw.Children[0]
	return nil
}

func decodeLanguage(lang [3]byte) string {
	// ISO-639-2/T packed 5 bits per letter.
	if lang[0] == 0 && lang[1] == 0 && lang[2] == 0 {
		return ""
	}
	out := make([]byte, 3)
	out[0] = (lang[0]&0x7C)>>2 + 0x60
	out[1] = (lang[0]&0x03)<<3 | (lang[1]&0xE0)>>5 + 0x60
	out[2] = (lang[1] & 0x1F) + 0x60
	return string(out)
}
