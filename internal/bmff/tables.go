package bmff

import (
	"encoding/binary"

	"mediad/internal/model"
)

const DefaultMaxSamples = 200_000

func u16(b []byte, o int) uint16 { return binary.BigEndian.Uint16(b[o : o+2]) }
func u32(b []byte, o int) uint32 { return binary.BigEndian.Uint32(b[o : o+4]) }
func u64(b []byte, o int) uint64 { return binary.BigEndian.Uint64(b[o : o+8]) }

func (s *Reader) parseStbl(stbl *BoxHeader, t *Track) error {
	if s.MaxSamples == 0 {
		s.MaxSamples = DefaultMaxSamples
	}
	stsd, err := s.MustChild(stbl, "stsd")
	if err != nil {
		return err
	}
	if err := s.parseStsd(stsd, t); err != nil {
		return err
	}

	durations, err := s.parseSTTS(stbl)
	if err != nil {
		return err
	}
	cts, err := s.parseCTTS(stbl, len(durations))
	if err != nil {
		return err
	}
	sizes, err := s.parseSTSZ(stbl)
	if err != nil {
		return err
	}
	chunks, err := s.parseChunkOffsets(stbl)
	if err != nil {
		return err
	}
	stsc, err := s.parseSTSC(stbl)
	if err != nil {
		return err
	}
	syncSet, err := s.parseSTSS(stbl)
	if err != nil {
		return err
	}
	if len(durations) != len(sizes) {
		return model.Newf(model.CodeCorruptBox, model.StageTables,
			"stts samples %d != stsz samples %d", len(durations), len(sizes)).
			WithSource(s.Source).WithTrack(t.ID).WithBox("stts")
	}
	if len(durations) > s.MaxSamples {
		return model.Newf(model.CodeUnsupported, model.StageTables, "sample count %d exceeds limit", len(durations)).
			WithSource(s.Source).WithTrack(t.ID)
	}
	offsets, err := s.assignSampleOffsets(t, sizes, chunks, stsc)
	if err != nil {
		return err
	}

	var dts int64
	for i := range durations {
		ct := int64(0)
		if i < len(cts) {
			ct = cts[i]
		}
		sync := true
		if syncSet != nil {
			_, sync = syncSet[uint32(i+1)]
		}
		t.Samples = append(t.Samples, Sample{
			Index:    i,
			TrackID:  t.ID,
			Offset:   offsets[i],
			Size:     sizes[i],
			DTS:      dts,
			PTS:      dts + ct,
			Duration: durations[i],
			Sync:     sync,
		})
		dts += int64(durations[i])
	}
	t.Duration = uint64(dts)
	return nil
}

func (s *Reader) parseStsd(b *BoxHeader, t *Track) error {
	payload, err := s.ReadPayload(b)
	if err != nil {
		return err
	}
	if len(payload) < 8 {
		return corruptBox(s.Source, b, "stsd payload too short")
	}
	count := u32(payload, 4)
	if count == 0 {
		return corruptBox(s.Source, b, "stsd has no sample entry")
	}
	entries, err := s.Range(b.PayloadOffset+8, b.End, b.Depth+1)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return corruptBox(s.Source, b, "stsd contains no entries")
	}
	e := entries[0]
	t.Codec = e.Type
	raw, err := s.ReadFull(&e)
	if err != nil {
		return err
	}
	t.SampleEntry = raw
	if e.Type == "encv" || e.Type == "enca" || e.Type == "encd" {
		t.Encrypted = true
		sinf, err := s.FirstChild(&e, "sinf")
		if err != nil {
			return err
		}
		if sinf != nil {
			schm, _ := s.FirstChild(sinf, "schm")
			if schm != nil {
				p, perr := s.ReadPayload(schm)
				if perr == nil && len(p) >= 8 {
					t.EncryptionScheme = string(p[4:8])
				}
			}
		}
	}
	return nil
}

func (s *Reader) parseSTTS(stbl *BoxHeader) ([]uint32, error) {
	b, err := s.MustChild(stbl, "stts")
	if err != nil {
		return nil, err
	}
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	if len(p) < 8 {
		return nil, corruptBox(s.Source, b, "short stts")
	}
	count := u32(p, 4)
	pos := 8
	var out []uint32
	for i := uint32(0); i < count; i++ {
		if pos+8 > len(p) {
			return nil, corruptBox(s.Source, b, "truncated stts entry %d", i)
		}
		n, delta := u32(p, pos), u32(p, pos+4)
		pos += 8
		if len(out)+int(n) > s.MaxSamples {
			return nil, model.New(model.CodeUnsupported, model.StageTables, "stts expands beyond sample limit").
				WithSource(s.Source).WithBox("stts")
		}
		for j := uint32(0); j < n; j++ {
			out = append(out, delta)
		}
	}
	return out, nil
}

func (s *Reader) parseCTTS(stbl *BoxHeader, n int) ([]int64, error) {
	b, err := s.FirstChild(stbl, "ctts")
	if err != nil || b == nil {
		return nil, err
	}
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	version, _, err := FullBoxVersionFlags(p)
	if err != nil || len(p) < 8 {
		return nil, corruptBox(s.Source, b, "short ctts")
	}
	count := u32(p, 4)
	pos := 8
	out := make([]int64, 0, n)
	for i := uint32(0); i < count; i++ {
		if pos+8 > len(p) {
			return nil, corruptBox(s.Source, b, "truncated ctts entry")
		}
		cnt := u32(p, pos)
		var off int64
		if version == 1 {
			off = int64(int32(u32(p, pos+4)))
		} else {
			off = int64(u32(p, pos+4))
		}
		pos += 8
		for j := uint32(0); j < cnt; j++ {
			out = append(out, off)
		}
	}
	return out, nil
}

func (s *Reader) parseSTSZ(stbl *BoxHeader) ([]uint32, error) {
	b, err := s.MustChild(stbl, "stsz")
	if err != nil {
		return nil, err
	}
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	if len(p) < 12 {
		return nil, corruptBox(s.Source, b, "short stsz")
	}
	uniform := u32(p, 4)
	count := u32(p, 8)
	if int(count) > s.MaxSamples {
		return nil, model.New(model.CodeUnsupported, model.StageTables, "stsz count exceeds sample limit").
			WithSource(s.Source).WithBox("stsz")
	}
	out := make([]uint32, count)
	if uniform != 0 {
		for i := range out {
			out[i] = uniform
		}
		return out, nil
	}
	pos := 12
	for i := range out {
		if pos+4 > len(p) {
			return nil, corruptBox(s.Source, b, "truncated stsz entry")
		}
		out[i] = u32(p, pos)
		pos += 4
	}
	return out, nil
}
