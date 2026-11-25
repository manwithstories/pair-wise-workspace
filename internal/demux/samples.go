package demux

import (
	"errors"
	"os"

	"archivecheck/internal/media"
)

// assembleSamples expands stsc/stco/stsz/stts into ordered, located samples,
// stopping cleanly at a truncated mdat while retaining every complete sample.
func assembleSamples(f *os.File, segment string, trackNo int, meta TrackMeta,
	children map[string]rawBox, fileSize, mdatEnd int64, cfg ParseConfig,
	faults *media.FaultSet, report *SourceReport) ([]Sample, error) {

	stszBox, ok := children["stsz"]
	if !ok {
		faults.Add(media.NewFault(media.KindMissingIndex, segment, trackNo, trakOff(children), "stsz sample-size table missing"))
		return nil, errors.New("missing stsz")
	}
	stszBytes, err := readPayloadAt(f, stszBox.payload, stszBox.end-stszBox.payload, cfg.MaxSampleBytes)
	if err != nil {
		faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, stszBox.payload, "unreadable stsz: %s", err))
		return nil, err
	}
	sizeTbl, uniform, err := parseStsz(stszBytes)
	if err != nil {
		faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, stszBox.payload, "%s", err))
		return nil, err
	}
	offBox, wide := children["stco"], false
	if b, ok := children["co64"]; ok {
		offBox, wide = b, true
	}
	offBytes, err := readPayloadAt(f, offBox.payload, offBox.end-offBox.payload, cfg.MaxSampleBytes)
	if err != nil {
		faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, offBox.payload, "unreadable offset table: %s", err))
		return nil, err
	}
	chunkOff, err := parseOffsets(offBytes, wide)
	if err != nil {
		faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, offBox.payload, "%s", err))
		return nil, err
	}
	stscBytes, _ := readPayloadAt(f, children["stsc"].payload, children["stsc"].end-children["stsc"].payload, cfg.MaxSampleBytes)
	stscTbl, err := parseStsc(stscBytes)
	if err != nil {
		faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, children["stsc"].payload, "%s", err))
		return nil, err
	}

	var deltas []uint32
	if stts, ok := children["stts"]; ok {
		if b, e := readPayloadAt(f, stts.payload, stts.end-stts.payload, cfg.MaxSampleBytes); e == nil {
			deltas, _ = parseStts(b)
		}
	}

	var samples []Sample
	var dts int64
	sampleIdx := 0
	for chunk, base := range chunkOff {
		spc := samplesPerChunk(stscTbl, uint32(chunk)+1)
		for j := uint32(0); j < spc; j++ {
			size := uniform
			if uniform == 0 {
				if sampleIdx >= len(sizeTbl) {
					faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, stszBox.payload,
						"stsc/stsz disagree: sample %d past size table", sampleIdx))
					return samples, errors.New("sample table mismatch")
				}
				size = sizeTbl[sampleIdx]
			}
			if int64(size) > cfg.MaxSampleBytes {
				faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, base,
					"sample length %d exceeds cap %d", size, cfg.MaxSampleBytes))
				return samples, errors.New("sample length cap")
			}
			payloadEnd := base + int64(size)
			if payloadEnd > mdatEnd || payloadEnd > fileSize {
				faults.Add(media.NewFault(media.KindTruncated, segment, trackNo, base,
					"mdat truncated at sample %d, retaining %d complete samples", sampleIdx, sampleIdx))
				report.Truncated = true
				return samples, nil
			}
			var dur int64
			if sampleIdx < len(deltas) {
				dur = int64(deltas[sampleIdx])
			}
			if err := media.CheckDuration(dur); err != nil {
				faults.Add(media.NewFault(media.KindZeroDuration, segment, trackNo, base,
					"sample %d has zero duration", sampleIdx))
			}
			if err := media.CheckMonotonic(0, dts); err != nil && sampleIdx > 0 {
				faults.Add(media.NewFault(media.KindWrap, segment, trackNo, base, "non-monotonic dts at sample %d", sampleIdx))
			}
			data := make([]byte, size)
			if _, err := f.ReadAt(data, base); err != nil {
				faults.Add(media.NewFault(media.KindTruncated, segment, trackNo, base, "payload read failed: %s", err))
				report.Truncated = true
				return samples, nil
			}
			samples = append(samples, Sample{
				Track: trackNo, Index: sampleIdx, Data: data, Offset: base,
				Duration: dur, DTS: dts, Codec: meta.Codec, TimeBase: meta.TimeBase,
			})
			dts += dur
			base = payloadEnd
			sampleIdx++
		}
	}
	return samples, nil
}

func trakOff(m map[string]rawBox) int64 {
	if b, ok := m["stsz"]; ok {
		return b.payload
	}
	return -1
}

// samplesPerChunk resolves the stsc run owning a 1-based chunk index.
func samplesPerChunk(entries []stscEntry, chunk uint32) uint32 {
	spc := uint32(1)
	for _, e := range entries {
		if e.FirstChunk <= chunk {
			spc = e.SamplesPerChunk
		}
	}
	return spc
}
