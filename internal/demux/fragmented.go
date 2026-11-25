package demux

import (
	"encoding/binary"
	"errors"
	"os"

	"archivecheck/internal/media"
)

// frag is a matched moof/mdat pair at the top level.
type frag struct {
	moof rawBox
	mdat rawBox
	have bool
}

// tstate accumulates one track across movie fragments.
type tstate struct {
	meta    TrackMeta
	samples []Sample
	dts     int64
}

// parseFragmented walks every moof/mdat movie fragment. trun tables drive
// sample placement; a fragment whose declared samples cannot be fully read is
// reported as incomplete and contributes NO keyframes (it must never be
// mis-indexed as a sync point).
func parseFragmented(f *os.File, segment string, fileSize int64, toplevel map[string]rawBox,
	cfg ParseConfig, faults *media.FaultSet, report *SourceReport) []TrackSamples {

	// First pass: ordered top-level fragment pairs (moof, mdat).
	var frags []frag
	_ = walkBoxes(f, 0, fileSize, segment, faults, func(b rawBox) error {
		switch b.info.Type.String() {
		case "moof":
			frags = append(frags, frag{moof: b})
		case "mdat":
			if len(frags) > 0 && !frags[len(frags)-1].have {
				frags[len(frags)-1].mdat = b
				frags[len(frags)-1].have = true
			}
		}
		_, _ = f.Seek(b.end, os.SEEK_SET)
		return nil
	})

	// Track state keyed by 1-based track id.
	tracks := map[uint32]*tstate{}

	var scanBytes int64
	for fi, fr := range frags {
		scanBytes += fr.moof.end - fr.moof.payload
		if scanBytes > cfg.ScanLimit {
			faults.Add(media.NewFault(media.KindScanLimit, segment, 0, fr.moof.payload,
				"fragment scan exceeded %d byte cap", cfg.ScanLimit))
			report.ScanLimited = true
			break
		}
		trafs, _ := collectBoxes(f, fr.moof, "traf")
		for _, traf := range trafs {
			trackID, baseDTS, truns := readTraf(f, traf)
			st, ok := tracks[trackID]
			if !ok {
				st = &tstate{meta: TrackMeta{
					Track: int(trackID), Codec: CodecAVC1, Handler: "vide",
					TimeBase: media.TimeBaseVideo90k,
				}}
				tracks[trackID] = st
			}
			if baseDTS > st.dts {
				st.dts = baseDTS
			}
			for _, tr := range truns {
				if readTrunSamples(f, segment, st, fr, fi+1, tr, cfg, faults, report) {
					// Complete fragment: keyframes already marked from trun flags.
				}
			}
		}
	}

	out := make([]TrackSamples, 0, len(tracks))
	for _, st := range tracks {
		st.meta.Samples = len(st.samples)
		report.Samples += len(st.samples)
		out = append(out, TrackSamples{Meta: st.meta, Samples: st.samples})
	}
	return out
}

type trunData struct {
	flags      uint32
	sampleCount uint32
	dataOffset int32
	sizes      []uint32
	flagsVals  []uint32
	durations  []uint32
}

// readTraf extracts tfhd track id, tfdt base dts and every trun in a traf.
func readTraf(f *os.File, traf rawBox) (trackID uint32, baseDTS int64, truns []trunData) {
	children := map[string]rawBox{}
	_ = walkBoxes(f, traf.payload, traf.end, "", &media.FaultSet{}, func(b rawBox) error {
		children[b.info.Type.String()] = b
		_, _ = f.Seek(b.end, os.SEEK_SET)
		return nil
	})
	if tfhd, ok := children["tfhd"]; ok {
		if b, e := readPayloadAt(f, tfhd.payload, tfhd.end-tfhd.payload, 4096); e == nil && len(b) >= 8 {
			trackID = binary.BigEndian.Uint32(b[4:8])
		}
	}
	if tfdt, ok := children["tfdt"]; ok {
		if b, e := readPayloadAt(f, tfdt.payload, tfdt.end-tfdt.payload, 4096); e == nil && len(b) >= 8 {
			if b[0] == 1 && len(b) >= 12 {
				baseDTS = int64(binary.BigEndian.Uint64(b[4:12]))
			} else {
				baseDTS = int64(binary.BigEndian.Uint32(b[4:8]))
			}
		}
	}
	if tr, ok := children["trun"]; ok {
		if b, e := readPayloadAt(f, tr.payload, tr.end-tr.payload, 1<<20); e == nil {
			if d, e2 := parseTrun(b); e2 == nil {
				truns = append(truns, d)
			}
		}
	}
	return
}

// parseTrun decodes a trun box honoring the common optional-field flags.
func parseTrun(b []byte) (trunData, error) {
	if len(b) < 8 {
		return trunData{}, errors.New("trun too short")
	}
	flags := binary.BigEndian.Uint32(b[0:4]) & 0x00FFFFFF
	n := binary.BigEndian.Uint32(b[4:8])
	d := trunData{flags: flags, sampleCount: n}
	pos := 8
	const (
		dataOffsetPresent = 0x000001
		sampleFlagsPresent = 0x000400
		sampleDurationPresent = 0x000100
		sampleSizePresent = 0x000200
	)
	if flags&dataOffsetPresent != 0 {
		if pos+4 > len(b) {
			return d, errors.New("trun data-offset truncated")
		}
		d.dataOffset = int32(binary.BigEndian.Uint32(b[pos:]))
		pos += 4
	}
	perSample := 0
	if flags&sampleDurationPresent != 0 {
		perSample += 4
	}
	if flags&sampleSizePresent != 0 {
		perSample += 4
	}
	if flags&sampleFlagsPresent != 0 {
		perSample += 4
	}
	for i := uint32(0); i < n; i++ {
		if pos+perSample > len(b) {
			return d, errors.New("trun samples truncated")
		}
		if flags&sampleDurationPresent != 0 {
			d.durations = append(d.durations, binary.BigEndian.Uint32(b[pos:]))
			pos += 4
		}
		if flags&sampleSizePresent != 0 {
			d.sizes = append(d.sizes, binary.BigEndian.Uint32(b[pos:]))
			pos += 4
		}
		if flags&sampleFlagsPresent != 0 {
			d.flagsVals = append(d.flagsVals, binary.BigEndian.Uint32(b[pos:]))
			pos += 4
		}
	}
	return d, nil
}

// readTrunSamples materializes the samples of one trun. It returns false and
// records an incomplete-fragment fault if any declared sample is missing.
func readTrunSamples(f *os.File, segment string, st *tstate, fr frag,
	fragNo int, tr trunData, cfg ParseConfig, faults *media.FaultSet, report *SourceReport) bool {

	mdatPayload := fr.mdat.payload
	base := mdatPayload + int64(tr.dataOffset)
	if tr.dataOffset == 0 {
		base = mdatPayload
	}
	var kept []Sample
	cur := base
	for i := uint32(0); i < tr.sampleCount; i++ {
		if int(i) >= len(tr.sizes) {
			faults.Add(media.NewFault(media.KindIncompleteFrag, segment, st.meta.Track, cur,
				"fragment %d trun missing size for sample %d; no keyframe claimed", fragNo, i))
			return false
		}
		size := int64(tr.sizes[i])
		if size > cfg.MaxSampleBytes || cur+size > fr.mdat.end {
			faults.Add(media.NewFault(media.KindIncompleteFrag, segment, st.meta.Track, cur,
				"fragment %d incomplete at sample %d/%d; partial fragment not indexed", fragNo, i, tr.sampleCount))
			return false
		}
		data := make([]byte, size)
		if _, err := f.ReadAt(data, cur); err != nil {
			faults.Add(media.NewFault(media.KindTruncated, segment, st.meta.Track, cur, "%s", err))
			return false
		}
		var dur int64 = 1
		if i < uint32(len(tr.durations)) {
			dur = int64(tr.durations[i])
		}
		key := true
		if i < uint32(len(tr.flagsVals)) {
			// trun sample flags: bits 0x00010000 = sample_depends_on(2).
			// Value 2 means the sample depends on others (not a sync sample).
			key = tr.flagsVals[i]&0x00030000 != 0x00020000
		}
		kept = append(kept, Sample{
			Track: st.meta.Track, Index: len(st.samples) + len(kept), Data: data,
			Offset: cur, Duration: dur, DTS: st.dts, Keyframe: key, Fragment: fragNo,
			Codec: st.meta.Codec, TimeBase: st.meta.TimeBase,
		})
		st.dts += dur
		cur += size
	}
	st.samples = append(st.samples, kept...)
	return true
}
