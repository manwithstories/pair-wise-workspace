package index

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediascan/internal/clock"
)

// parseMoov reads track tables and converts sample offsets into byte ranges.
func (s *source) parseMoov(moov *mp4.BoxInfo, mdats [][2]int64) error {
	traks, err := s.extractBoxes(moov, mp4.BoxTypeTrak())
	if err != nil {
		return fmt.Errorf("%w: trak: %v", ErrUnsupported, err)
	}
	for _, trak := range traks {
		ts, err := s.parseTrak(trak, mdats)
		if err != nil {
			return err
		}
		if ts != nil {
			s.tracks = append(s.tracks, *ts)
		}
	}
	return nil
}

func (s *source) parseTrak(trak *mp4.BoxInfo, mdats [][2]int64) (*trackSource, error) {
	ts := &trackSource{}
	mdhd, err := s.payload(trak, mp4.BoxTypeMdhd(), mp4.BoxTypeMdia())
	if err != nil {
		return nil, fmt.Errorf("%w: mdhd: %v", ErrUnsupported, err)
	}
	if m, ok := mdhd.(*mp4.Mdhd); ok && m != nil {
		ts.timescale = m.Timescale
	}
	if ts.timescale == 0 {
		ts.timescale = clock.DefaultTimescale
	}
	hdlr, err := s.payload(trak, mp4.BoxTypeHdlr(), mp4.BoxTypeMdia())
	if err != nil {
		return nil, fmt.Errorf("%w: hdlr: %v", ErrUnsupported, err)
	}
	if h, ok := hdlr.(*mp4.Hdlr); ok && h != nil {
		ts.handler = string(h.HandlerType[:])
	}
	tkhd, err := s.payload(trak, mp4.BoxTypeTkhd())
	if err != nil {
		return nil, fmt.Errorf("%w: tkhd: %v", ErrUnsupported, err)
	}
	if t, ok := tkhd.(*mp4.Tkhd); ok && t != nil {
		ts.id = t.TrackID
	}
	if err := s.parseStbl(ts, trak, mdats); err != nil {
		return nil, err
	}
	return ts, nil
}

func (s *source) parseStbl(ts *trackSource, trak *mp4.BoxInfo, mdats [][2]int64) error {
	stblPath := []mp4.BoxType{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()}
	stsz, err := s.payload(trak, mp4.BoxTypeStsz(), stblPath...)
	if err != nil {
		return fmt.Errorf("%w: stsz: %v", ErrUnsupported, err)
	}
	stts, err := s.payload(trak, mp4.BoxTypeStts(), stblPath...)
	if err != nil {
		return fmt.Errorf("%w: stts: %v", ErrUnsupported, err)
	}
	stsc, err := s.payload(trak, mp4.BoxTypeStsc(), stblPath...)
	if err != nil {
		return fmt.Errorf("%w: stsc: %v", ErrUnsupported, err)
	}
	stco, err := s.payload(trak, mp4.BoxTypeStco(), stblPath...)
	if err != nil {
		return fmt.Errorf("%w: stco: %v", ErrUnsupported, err)
	}
	ctts, err := s.payload(trak, mp4.BoxTypeCtts(), stblPath...)
	if err != nil {
		return fmt.Errorf("%w: ctts: %v", ErrUnsupported, err)
	}
	stss, err := s.payload(trak, mp4.BoxTypeStss(), stblPath...)
	if err != nil {
		return fmt.Errorf("%w: stss: %v", ErrUnsupported, err)
	}
	sz, ok := stsz.(*mp4.Stsz)
	if !ok || sz == nil {
		return nil // no progressive samples; fragments may still supply them
	}
	offsets, ok := chunkOffsets(stco)
	if !ok {
		return nil
	}
	sizes := sampleSizes(sz)
	if len(sizes) == 0 {
		return nil
	}
	deltas := sampleDeltas(stts)
	offs, err := sampleOffsets(offsets, stsc, sizes, int64(s.size), mdats)
	if err != nil {
		return err
	}
	syncSet := syncSamples(stss, len(sizes))
	var tNs int64
	for i := 0; i < len(sizes) && i < len(offs); i++ {
		dur := deltas[i]
		ts.samples = append(ts.samples, sampleRef{
			offset: offs[i],
			size:   int64(sizes[i]),
			timeNs: tNs,
			durNs:  clock.NanosDelta(int64(dur), ts.timescale),
			sync:   syncSet[i],
		})
		tNs += clock.NanosDelta(int64(dur), ts.timescale)
	}
	_ = ctts
	return nil
}

// index / verify / output helpers live in index.go.
