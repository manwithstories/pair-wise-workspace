package demux

import (
	"bytes"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// parseTrak extracts one track's metadata and sample table.
func (c *Container) parseTrak(moov io.ReaderAt, trak boxHeader, file io.ReaderAt) (*Track, error) {
	payload, err := readPayload(moov, trak, maxMoovBytes)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(payload)
	t := &Track{IndexComplete: true}
	var in tableInput
	var mdia *boxHeader
	err = walkBoxes(r, 0, int64(len(payload)), func(bh boxHeader) error {
		switch bh.typ {
		case "tkhd":
			var tkhd mp4.Tkhd
			if err := readInto(r, bh, &tkhd); err != nil {
				return err
			}
			t.ID = tkhd.TrackID
		case "mdia":
			b := bh
			mdia = &b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if mdia == nil {
		return nil, errMissing("mdia")
	}
	if err := c.parseMdia(r, *mdia, t, &in); err != nil {
		return nil, err
	}
	t.Samples = buildSamples(in, c.Faults, t.ID, t.Timescale, c.Size)
	if t.Kind == "video" && in.stss == nil {
		c.recoverKeyframes(file, t)
	}
	c.Missing = append(c.Missing, missingIntervals(t.Samples, t.Timescale)...)
	return t, nil
}

// parseMdia walks mdia collecting timescale, handler type and minf.
func (c *Container) parseMdia(trakPayload io.ReaderAt, mdia boxHeader, t *Track, in *tableInput) error {
	payload, err := readPayload(trakPayload, mdia, maxMoovBytes)
	if err != nil {
		return err
	}
	r := bytes.NewReader(payload)
	var minf *boxHeader
	err = walkBoxes(r, 0, int64(len(payload)), func(bh boxHeader) error {
		switch bh.typ {
		case "mdhd":
			var mdhd mp4.Mdhd
			if err := readInto(r, bh, &mdhd); err != nil {
				return err
			}
			t.Timescale = mdhd.Timescale
		case "hdlr":
			var hdlr mp4.Hdlr
			if err := readInto(r, bh, &hdlr); err != nil {
				return err
			}
			switch string(hdlr.HandlerType[:]) {
			case "vide":
				t.Kind = "video"
			case "soun":
				t.Kind = "audio"
			default:
				t.Kind = "other"
			}
		case "minf":
			b := bh
			minf = &b
		}
		return nil
	})
	if err != nil {
		return err
	}
	if minf == nil {
		return errMissing("minf")
	}
	return c.parseMinf(r, *minf, t, in)
}

// parseMinf descends into the stbl sample table.
func (c *Container) parseMinf(mdiaPayload io.ReaderAt, minf boxHeader, t *Track, in *tableInput) error {
	payload, err := readPayload(mdiaPayload, minf, maxMoovBytes)
	if err != nil {
		return err
	}
	r := bytes.NewReader(payload)
	return walkBoxes(r, 0, int64(len(payload)), func(bh boxHeader) error {
		if bh.typ != "stbl" {
			return nil
		}
		stbl, err := readPayload(r, bh, maxMoovBytes)
		if err != nil {
			return err
		}
		return parseStbl(bytes.NewReader(stbl), int64(len(stbl)), t, in, c.Faults)
	})
}
