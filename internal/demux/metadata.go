package demux

import (
	"fmt"

	"archivecheck.local/archivecheck/internal/ebml"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/track"
)

func (s *scanner) readInfo(e ebml.Element) error {
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, "Info", fault.ErrMalformed)
	}
	s.result.Info.TimestampScale = 1_000_000
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return err
		}
		switch c.ID {
		case ebml.IDTimestampScale:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return err
			}
			if v == 0 {
				return fault.AtElement(fault.StageParse, c.Offset, "TimestampScale", fault.ErrMalformed)
			}
			s.result.Info.TimestampScale = int64(v)
		case ebml.IDDuration:
			b, err := s.r.ReadSmall(c, 8)
			if err != nil {
				return err
			}
			d, err := ebml.Float64(b)
			if err != nil {
				return fault.AtElement(fault.StageParse, c.Offset, "Duration", fault.ErrMalformed)
			}
			s.result.Info.Duration = d
		case ebml.IDMuxingApp:
			b, err := s.r.ReadSmall(c, 256)
			if err != nil {
				return err
			}
			s.result.Info.MuxingApp = string(b)
		case ebml.IDWritingApp:
			b, err := s.r.ReadSmall(c, 256)
			if err != nil {
				return err
			}
			s.result.Info.WritingApp = string(b)
		default:
			if err := s.r.Skip(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) readTracks(e ebml.Element) error {
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, "Tracks", fault.ErrMalformed)
	}
	seen := map[uint64]bool{}
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return err
		}
		if c.ID != ebml.IDTrackEntry {
			if err := s.r.Skip(c); err != nil {
				return err
			}
			continue
		}
		src, err := s.readTrackEntry(c)
		if err != nil {
			return err
		}
		if seen[src.Number] {
			return fault.AtElement(fault.StageParse, c.Offset, "TrackEntry",
				fmt.Errorf("%w: duplicate TrackNumber %d", fault.ErrMalformed, src.Number)).OnTrack(src.Number)
		}
		seen[src.Number] = true
		s.result.Tracks = append(s.result.Tracks, src)
		if s.opts.OnTrack != nil {
			s.opts.OnTrack(src)
		}
	}
	return nil
}

func (s *scanner) readTrackEntry(e ebml.Element) (track.Source, error) {
	var src track.Source
	if e.UnknownSize {
		return src, fault.AtElement(fault.StageParse, e.Offset, "TrackEntry", fault.ErrMalformed)
	}
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return src, err
		}
		switch c.ID {
		case ebml.IDTrackNumber:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return src, err
			}
			src.Number = v
		case ebml.IDTrackUID:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return src, err
			}
			src.UID = v
		case ebml.IDTrackType:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return src, err
			}
			src.Type = track.Type(v)
		case ebml.IDCodecID:
			b, err := s.r.ReadSmall(c, 128)
			if err != nil {
				return src, err
			}
			src.CodecID = string(b)
		case ebml.IDCodecName:
			b, err := s.r.ReadSmall(c, 256)
			if err != nil {
				return src, err
			}
			src.CodecName = string(b)
		case ebml.IDName:
			b, err := s.r.ReadSmall(c, 256)
			if err != nil {
				return src, err
			}
			src.Name = string(b)
		case ebml.IDLanguage:
			b, err := s.r.ReadSmall(c, 32)
			if err != nil {
				return src, err
			}
			src.Language = string(b)
		case ebml.IDDefaultDuration:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return src, err
			}
			src.DefaultDuration = v
		case ebml.IDFlagLacing:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return src, err
			}
			src.FlagLacing = v != 0
		case ebml.IDCodecPrivate:
			if c.UnknownSize {
				return src, fault.AtElement(fault.StageParse, c.Offset, "CodecPrivate", fault.ErrMalformed)
			}
			buf, err := s.r.ReadSmall(c, s.opts.MaxCodecPrivateBytes)
			if err != nil {
				return src, err
			}
			src.CodecPrivate = append([]byte(nil), buf...)
		case ebml.IDVideo:
			if err := s.readVideo(c, &src); err != nil {
				return src, err
			}
		case ebml.IDAudio:
			if err := s.readAudio(c, &src); err != nil {
				return src, err
			}
		default:
			if err := s.r.Skip(c); err != nil {
				return src, err
			}
		}
	}
	if src.Number == 0 {
		return src, fault.AtElement(fault.StageParse, e.Offset, "TrackEntry", fault.ErrMalformed)
	}
	return src, nil
}

func (s *scanner) readVideo(e ebml.Element, src *track.Source) error {
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, "Video", fault.ErrMalformed)
	}
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return err
		}
		switch c.ID {
		case ebml.IDPixelWidth:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return err
			}
			src.PixelWidth = v
		case ebml.IDPixelHeight:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return err
			}
			src.PixelHeight = v
		default:
			if err := s.r.Skip(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) readAudio(e ebml.Element, src *track.Source) error {
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, "Audio", fault.ErrMalformed)
	}
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return err
		}
		switch c.ID {
		case ebml.IDSamplingFrequency:
			b, err := s.r.ReadSmall(c, 8)
			if err != nil {
				return err
			}
			f, err := ebml.Float64(b)
			if err != nil {
				return fault.AtElement(fault.StageParse, c.Offset, "SamplingFrequency", fault.ErrMalformed)
			}
			src.SamplingFrequency = uint64(f)
		case ebml.IDChannels:
			v, err := s.r.ReadUint(c)
			if err != nil {
				return err
			}
			src.Channels = v
		default:
			if err := s.r.Skip(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) readCues(e ebml.Element) error {
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, "Cues", fault.ErrMalformed)
	}
	s.result.Info.HasCues = true
	for s.r.Offset() < e.End {
		c, err := s.r.Next(e.End)
		if err != nil {
			return err
		}
		if c.ID == ebml.IDCuePoint {
			s.result.Info.CueEntries++
		}
		if err := s.r.Skip(c); err != nil {
			return err
		}
	}
	return nil
}
