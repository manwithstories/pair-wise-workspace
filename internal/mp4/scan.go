// Package mp4 scans local ISOBMFF files: track time bases, sample tables,
// keyframes and source byte offsets.
package mp4

import (
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

func scanTrack(r io.ReaderAt, trak box, all []box, size int64) (Track, error) {
	prefix := strPath(trak.path) + "/"
	get := func(rel string) *box {
		bs := selectBoxes(all, prefix+rel)
		if len(bs) == 0 {
			return nil
		}
		return &bs[0]
	}

	tr := Track{SourceBytes: size}
	mdhd := get("mdia/mdhd")
	if mdhd == nil {
		return tr, fmt.Errorf("mdhd missing")
	}
	var m mp4.Mdhd
	if err := unmarshal(r, *mdhd, &m); err != nil {
		return tr, fmt.Errorf("mdhd: %w", err)
	}
	if tkhd := get("tkhd"); tkhd != nil {
		var t mp4.Tkhd
		if err := unmarshal(r, *tkhd, &t); err == nil {
			tr.ID = t.TrackID
		}
	}
	tr.Timescale = m.Timescale
	tr.Duration = int64(m.GetDuration())
	if hdlr := get("mdia/hdlr"); hdlr != nil {
		var h mp4.Hdlr
		if err := unmarshal(r, *hdlr, &h); err == nil {
			tr.Kind = string(h.HandlerType[:])
		}
	}

	stbl := "mdia/minf/stbl/"
	if sd := get(stbl + "stsd"); sd != nil {
		tr.Codec = sampleEntryCodec(r, *sd)
	}

	var stts mp4.Stts
	if b := get(stbl + "stts"); b != nil {
		if err := unmarshal(r, *b, &stts); err != nil {
			return tr, fmt.Errorf("stts: %w", err)
		}
	}
	tr.SttsEntries = len(stts.Entries)

	var stsz mp4.Stsz
	if b := get(stbl + "stsz"); b != nil {
		if err := unmarshal(r, *b, &stsz); err != nil {
			return tr, fmt.Errorf("stsz: %w", err)
		}
	}
	sizes, err := sampleSizes(stsz)
	if err != nil {
		return tr, err
	}

	var stsc mp4.Stsc
	if b := get(stbl + "stsc"); b != nil {
		if err := unmarshal(r, *b, &stsc); err != nil {
			return tr, fmt.Errorf("stsc: %w", err)
		}
	}
	offsets, err := chunkOffsets(r, get(stbl+"stco"), get(stbl+"co64"))
	if err != nil {
		return tr, err
	}
	keyframes := map[uint32]bool{}
	hasKeyframes := false
	if b := get(stbl + "stss"); b != nil {
		var stss mp4.Stss
		if err := unmarshal(r, *b, &stss); err == nil {
			hasKeyframes = true
			for _, n := range stss.SampleNumber {
				keyframes[n] = true
			}
		}
	}

	tr.Samples = buildSamples(sizes, stts, stsc, offsets, keyframes, hasKeyframes)
	tr.ConstantFrame = constantDurations(stts)
	tr.KeyframeGaps = keyframeGaps(tr.Samples)
	tr.KeyframeGapP50 = quantile(tr.KeyframeGaps, 50)
	tr.KeyframeGapP95 = quantile(tr.KeyframeGaps, 95)

	flagCorruption(r, &tr, size)
	return tr, nil
}
