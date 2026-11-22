package stream

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediascan/internal/media"
)

func analyzeTrack(r io.ReadSeeker, t *trakBoxes, mdats []byteRange, limits Limits) (*Track, media.IssueSet, []media.Range) {
	var issues media.IssueSet
	if t.tkhd == nil || t.mdhd == nil {
		issues.Add(media.NewIssue(media.ClassMP4BadStructure, media.Locator{},
			"track is missing tkhd or mdhd"))
		return nil, issues, nil
	}
	tkhd := &mp4.Tkhd{}
	if err := unmarshalAt(r, t.tkhd, tkhd); err != nil {
		issues.Add(media.NewIssue(media.ClassMP4BadStructure, media.Locator{ByteOffset: int64(t.tkhd.Offset)},
			"cannot parse tkhd: %v", err))
		return nil, issues, nil
	}
	mdhd := &mp4.Mdhd{}
	if err := unmarshalAt(r, t.mdhd, mdhd); err != nil {
		issues.Add(media.NewIssue(media.ClassMP4BadStructure, media.Locator{ByteOffset: int64(t.mdhd.Offset)},
			"cannot parse mdhd: %v", err))
		return nil, issues, nil
	}
	tr := &Track{TrackID: tkhd.TrackID, Timescale: mdhd.Timescale, Kind: "video"}
	if t.hdlr != nil {
		hdlr := &mp4.Hdlr{}
		if err := unmarshalAt(r, t.hdlr, hdlr); err == nil {
			switch string(hdlr.HandlerType[:]) {
			case "vide":
				tr.Kind = "video"
			case "soun":
				tr.Kind = "audio"
			default:
				tr.Kind = "other"
			}
		}
	}
	if tr.Timescale == 0 {
		issues.Add(media.NewIssue(media.ClassMP4BadStructure, media.Locator{TrackID: tr.TrackID},
			"mdhd timescale is zero"))
		return tr, issues, nil
	}
	durNs, derr := media.ScaleToNanos(int64(mdhd.GetDuration()), tr.Timescale)
	if derr != nil {
		issues.Add(media.NewIssue(media.ClassMP4BadStructure, media.Locator{TrackID: tr.TrackID},
			"mdhd duration overflows: %v", derr))
	} else {
		tr.DurationNs = durNs
	}

	// Sample sizes. A corrupt stsz can declare a huge SampleCount with a tiny
	// payload (constant-size samples carry no per-sample entries for the
	// parser to bound against), so the count is checked against the limit
	// before any allocation.
	var sizes []uint32
	if t.stsz != nil {
		stsz := &mp4.Stsz{}
		if err := unmarshalAt(r, t.stsz, stsz); err == nil {
			if stsz.SampleSize != 0 {
				if int64(stsz.SampleCount) > int64(limits.MaxSamples) {
					issues.Add(media.NewFatal(media.ClassMP4BadStructure, media.Locator{TrackID: tr.TrackID, ByteOffset: int64(t.stsz.Offset)},
						"stsz declares %d constant-size samples, limit is %d", stsz.SampleCount, limits.MaxSamples))
					return tr, issues, nil
				}
				sizes = make([]uint32, stsz.SampleCount)
				for i := range sizes {
					sizes[i] = stsz.SampleSize
				}
			} else {
				sizes = stsz.EntrySize
			}
		}
	}
	// Sample durations. stts entries are (count, delta) pairs; validate the
	// summed count before expanding, for the same reason as stsz.
	var deltas []uint32
	if t.stts != nil {
		stts := &mp4.Stts{}
		if err := unmarshalAt(r, t.stts, stts); err == nil {
			var total int64
			for _, e := range stts.Entries {
				total += int64(e.SampleCount)
				if total > int64(limits.MaxSamples) {
					issues.Add(media.NewFatal(media.ClassMP4BadStructure, media.Locator{TrackID: tr.TrackID, ByteOffset: int64(t.stts.Offset)},
						"stts declares more than %d samples", limits.MaxSamples))
					return tr, issues, nil
				}
			}
			deltas = make([]uint32, 0, total)
			for _, e := range stts.Entries {
				for i := uint32(0); i < e.SampleCount; i++ {
					deltas = append(deltas, e.SampleDelta)
				}
			}
		}
	}
	// Chunk offsets.
	var chunks []uint64
	if t.stco != nil {
		stco := &mp4.Stco{}
		if err := unmarshalAt(r, t.stco, stco); err == nil {
			for _, o := range stco.ChunkOffset {
				chunks = append(chunks, uint64(o))
			}
		}
	} else if t.co64 != nil {
		co64 := &mp4.Co64{}
		if err := unmarshalAt(r, t.co64, co64); err == nil {
			chunks = append(chunks, co64.ChunkOffset...)
		}
	}
	// Samples per chunk.
	var stscEntries []mp4.StscEntry
	if t.stsc != nil {
		stsc := &mp4.Stsc{}
		if err := unmarshalAt(r, t.stsc, stsc); err == nil {
			stscEntries = stsc.Entries
		}
	}

	sampleCount := len(sizes)
	if len(deltas) < sampleCount {
		issues.Add(media.NewIssue(media.ClassMP4MissingIndex, media.Locator{TrackID: tr.TrackID},
			"stts covers %d samples but stsz declares %d", len(deltas), sampleCount))
	}
	if sampleCount > limits.MaxSamples {
		issues.Add(media.NewFatal(media.ClassMP4BadStructure, media.Locator{TrackID: tr.TrackID},
			"sample count %d exceeds limit %d", sampleCount, limits.MaxSamples))
		return tr, issues, nil
	}

	// Map sample number -> file offset via stsc/stco.
	offsets := sampleOffsets(sizes, stscEntries, chunks)
	tr.Complete = len(offsets) == sampleCount && len(deltas) >= sampleCount
	if len(offsets) < sampleCount {
		issues.Add(media.NewIssue(media.ClassMP4MissingIndex, media.Locator{TrackID: tr.TrackID},
			"chunk tables locate %d of %d samples", len(offsets), sampleCount))
	}

	// Keyframe numbers from stss; absent stss means every sample is a sync
	// sample per ISO/IEC 14496-12.
	syncSet := map[int]bool{}
	indexSource := "stss"
	if t.stss != nil {
		stss := &mp4.Stss{}
		if err := unmarshalAt(r, t.stss, stss); err == nil {
			for _, n := range stss.SampleNumber {
				syncSet[int(n)] = true
			}
		}
	} else {
		indexSource = "implicit-all-sync"
		for i := 1; i <= sampleCount; i++ {
			syncSet[i] = true
		}
	}
	// Independent scan: inspect each sample's payload for a keyframe marker.
	// The marker is a bit 0x80 in the first payload byte of a sample. Because
	// this signal comes from the sample bytes rather than the sample table, it
	// reveals when stss has lost entries. It is only meaningful for video and
	// only when the byte spans are readable.
	scanned := make(map[int]bool)
	scanValid := t.stss != nil && tr.Kind != "audio"
	if scanValid {
		for i := 0; i < sampleCount && i < len(offsets); i++ {
			if sizes[i] == 0 || !insideAnyMdat(mdats, uint64(offsets[i]), 1) {
				continue
			}
			var first [1]byte
			if _, err := r.Seek(offsets[i], io.SeekStart); err != nil {
				continue
			}
			if _, err := io.ReadFull(r, first[:]); err != nil {
				continue
			}
			if first[0]&0x80 != 0 {
				scanned[i+1] = true
			}
		}
	}
	// If the declared stss is a strict subset of the markers actually present
	// in the samples, the index is incomplete: restore the missing entries and
	// record that a rebuild occurred.
	if scanValid && len(scanned) > len(syncSet) {
		restored := 0
		for n := range scanned {
			if !syncSet[n] {
				syncSet[n] = true
				restored++
			}
		}
		issues.Add(media.NewIssue(media.ClassMP4MissingIndex, media.Locator{TrackID: tr.TrackID},
			"stss declared %d keyframes but samples mark %d; restored %d entries by scanning",
			len(scanned)-restored, len(scanned), restored))
		indexSource = "rebuilt"
	}
	tr.IndexSource = indexSource

	// Build the sample list with cumulative DTS in ticks then scaled to ns.
	var dtsTicks int64
	var corrupt []media.Range
	for i := 0; i < sampleCount; i++ {
		num := i + 1
		var delta uint32
		if i < len(deltas) {
			delta = deltas[i]
		}
		dtsNs, err := media.ScaleToNanos(dtsTicks, tr.Timescale)
		if err != nil {
			issues.Add(media.NewIssue(media.ClassMP4BadStructure, media.Locator{TrackID: tr.TrackID, ByteOffset: int64(t.stts.Offset)},
				"sample %d DTS overflow: %v", num, err))
			break
		}
		endNs, err := media.ScaleToNanos(dtsTicks+int64(delta), tr.Timescale)
		if err != nil {
			endNs = dtsNs
		}
		dtsTicks += int64(delta)

		s := Sample{Number: num, Size: int64(sizes[i]), DTSNs: dtsNs, EndNs: endNs, DurationNs: endNs - dtsNs, Sync: syncSet[num]}
		if i < len(offsets) {
			s.Offset = offsets[i]
			// Validate the sample's byte span lies inside some mdat.
			if s.Size <= 0 {
				issues.Add(media.NewIssue(media.ClassMP4CorruptSample, media.Locator{
					TrackID: tr.TrackID, TrackKind: tr.Kind, ByteOffset: s.Offset, ByteLength: s.Size, TimeNs: dtsNs,
				}, "sample %d has non-positive size %d", num, s.Size))
				corrupt = append(corrupt, media.Range{
					TrackID: tr.TrackID, TrackKind: tr.Kind, Class: media.ClassMP4CorruptSample,
					StartNs: dtsNs, EndNs: endNs, ByteStart: s.Offset, ByteEnd: s.Offset, Recoverable: true,
					Detail: fmt.Sprintf("sample %d size=%d", num, s.Size),
				})
			} else if !insideAnyMdat(mdats, uint64(s.Offset), uint64(s.Size)) {
				issues.Add(media.NewIssue(media.ClassMP4CorruptSample, media.Locator{
					TrackID: tr.TrackID, TrackKind: tr.Kind, ByteOffset: s.Offset, ByteLength: s.Size, TimeNs: dtsNs,
				}, "sample %d byte span [%d,%d) is outside every mdat", num, s.Offset, s.Offset+s.Size))
				corrupt = append(corrupt, media.Range{
					TrackID: tr.TrackID, TrackKind: tr.Kind, Class: media.ClassMP4CorruptSample,
					StartNs: dtsNs, EndNs: endNs, ByteStart: s.Offset, ByteEnd: s.Offset + s.Size, Recoverable: true,
					Detail: fmt.Sprintf("sample %d out of mdat", num),
				})
			}
		} else {
			issues.Add(media.NewIssue(media.ClassMP4MissingIndex, media.Locator{TrackID: tr.TrackID, TimeNs: dtsNs},
				"sample %d has no chunk offset (index incomplete)", num))
		}
		if s.Sync {
			tr.Keyframes = append(tr.Keyframes, num)
		}
		tr.Samples = append(tr.Samples, s)
	}
	if tr.Kind == "video" && len(tr.Keyframes) == 0 && sampleCount > 0 {
		issues.Add(media.NewIssue(media.ClassMP4NoKeyframes, media.Locator{TrackID: tr.TrackID},
			"track has %d samples but no sync sample", sampleCount))
	}
	return tr, issues, corrupt
}
