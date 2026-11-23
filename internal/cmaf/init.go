package cmaf

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediaflow/internal/clock"
)

// ParseInit incrementally parses a CMAF initialization segment. Only the moov
// and its immediate ancestors are decoded; mdat payloads are skipped as boxes.
// A structurally broken child is recorded as a local problem and scanning
// continues with later boxes.
func (s *Scanner) ParseInit(buf []byte, segIdx int) (InitSegment, error) {
	out := InitSegment{SegmentIndex: segIdx, Size: len(buf)}
	if len(buf) > s.Limits.MaxSegmentBytes {
		return out, fmt.Errorf("cmaf: init segment %d exceeds limit (%d > %d)", segIdx, len(buf), s.Limits.MaxSegmentBytes)
	}
	boxes, problems := WalkTopLevel(buf, s.Limits.MaxBoxBytes)
	for _, p := range problems {
		out.Problems = append(out.Problems, problemToLocated(p, segIdx))
	}
	moov, ok := findBox(boxes, "moov")
	if !ok {
		return out, fmt.Errorf("cmaf: init segment %d has no moov", segIdx)
	}
	moovKids, probs := children(buf, moov)
	for _, p := range probs {
		out.Problems = append(out.Problems, problemToLocated(p, segIdx))
	}

	traks := make(map[uint32]*TrackInfo)
	var order []uint32
	for _, trak := range moovKids {
		if trak.Type != "trak" {
			continue
		}
		ti, err := s.parseTrak(buf, trak, segIdx)
		if err != nil {
			out.Problems = append(out.Problems, clock.LocatedError{
				Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(trak.Offset), Detail: err.Error(),
			})
			continue
		}
		traks[ti.TrackID] = ti
		order = append(order, ti.TrackID)
	}

	for _, mv := range moovKids {
		if mv.Type != "mvex" {
			continue
		}
		mvKids, mprobs := children(buf, mv)
		for _, p := range mprobs {
			out.Problems = append(out.Problems, problemToLocated(p, segIdx))
		}
		for _, trexBox := range mvKids {
			if trexBox.Type != "trex" {
				continue
			}
			var trex mp4.Trex
			if err := decodeBox(buf, trexBox, &trex); err != nil {
				out.Problems = append(out.Problems, clock.LocatedError{
					Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(trexBox.Offset), Detail: err.Error(),
				})
				continue
			}
			if ti, ok := traks[trex.TrackID]; ok {
				ti.DefaultSampleDuration = trex.DefaultSampleDuration
				ti.DefaultSampleSize = trex.DefaultSampleSize
				ti.DefaultSampleFlags = trex.DefaultSampleFlags
			}
		}
	}

	for _, id := range order {
		out.Tracks = append(out.Tracks, *traks[id])
	}
	if len(out.Tracks) == 0 {
		return out, fmt.Errorf("cmaf: init segment %d has no usable track", segIdx)
	}
	return out, nil
}

func (s *Scanner) parseTrak(buf []byte, trak Box, segIdx int) (*TrackInfo, error) {
	kids, _ := children(buf, trak)
	ti := &TrackInfo{}
	for _, b := range kids {
		switch b.Type {
		case "tkhd":
			var tkhd mp4.Tkhd
			if err := decodeBox(buf, b, &tkhd); err != nil {
				return nil, err
			}
			ti.TrackID = tkhd.TrackID
			ti.Width = uint16(tkhd.Width >> 16)
			ti.Height = uint16(tkhd.Height >> 16)
			if tkhd.GetVersion() == 0 {
				ti.DurationTicks = uint64(tkhd.DurationV0)
			} else {
				ti.DurationTicks = tkhd.DurationV1
			}
		case "mdia":
			if err := s.parseMdia(buf, b, ti); err != nil {
				return nil, err
			}
		}
	}
	if ti.TrackID == 0 {
		return nil, fmt.Errorf("trak at %d has no tkhd track id", trak.Offset)
	}
	if ti.Timescale == 0 {
		return nil, fmt.Errorf("track %d has zero timescale", ti.TrackID)
	}
	return ti, nil
}

func (s *Scanner) parseMdia(buf []byte, mdia Box, ti *TrackInfo) error {
	kids, _ := children(buf, mdia)
	for _, b := range kids {
		switch b.Type {
		case "mdhd":
			var mdhd mp4.Mdhd
			if err := decodeBox(buf, b, &mdhd); err != nil {
				return err
			}
			ti.Timescale = mdhd.Timescale
		case "hdlr":
			var hdlr mp4.Hdlr
			if err := decodeBox(buf, b, &hdlr); err != nil {
				return err
			}
			ti.Handler = string(hdlr.HandlerType[:])
		case "minf":
			minfKids, _ := children(buf, b)
			for _, mb := range minfKids {
				if mb.Type != "stbl" {
					continue
				}
				stblKids, _ := children(buf, mb)
				for _, sb := range stblKids {
					if sb.Type != "stsd" {
						continue
					}
					entryKids, _ := childrenAfter(buf, sb, 8)
					for _, e := range entryKids {
						if e.Type != "avc1" && e.Type != "hvc1" && e.Type != "hev1" && e.Type != "mp4v" {
							continue
						}
						ti.Codec = e.Type
						var vse mp4.VisualSampleEntry
						vse.Type = mp4.StrToBoxType(e.Type)
						if err := decodeBox(buf, e, &vse); err == nil {
							ti.Width = vse.Width
							ti.Height = vse.Height
						}
						break
					}
				}
			}
		}
	}
	return nil
}
