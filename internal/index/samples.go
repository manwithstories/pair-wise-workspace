package index

import (
	"fmt"
	"os"

	"github.com/abema/go-mp4"

	"mediad/internal/model"
)

// payloadOf expands one leaf box and returns its typed payload.
func payloadOf(f *os.File, stbl *mp4.BoxInfo,
	t mp4.BoxType) (mp4.IBox, *mp4.BoxInfo, error) {
	boxes, err := mp4.ExtractBoxWithPayload(f, stbl, mp4.BoxPath{t})
	if err != nil {
		return nil, nil, err
	}
	if len(boxes) == 0 {
		return nil, nil, nil
	}
	return boxes[0].Payload, &boxes[0].Info, nil
}

// buildSamples expands stsc/stco(+co64)/stsz/stts/ctts/stss into flat
// samples and validates each [offset, offset+size) against the mdat range.
func buildSamples(f *os.File, stbl *mp4.BoxInfo,
	idx *FileIndex, tr *Track, addFault faultAdder) error {

	stscP, stscBI, err := payloadOf(f, stbl, mp4.BoxTypeStsc())
	if err != nil {
		return err
	}
	if stscP == nil {
		return model.NewFault(model.FaultMissingIndex, int64(stbl.Offset),
			idx.Name, "stbl missing stsc")
	}
	stszP, stszBI, err := payloadOf(f, stbl, mp4.BoxTypeStsz())
	if err != nil {
		return err
	}
	if stszP == nil {
		return model.NewFault(model.FaultMissingIndex, int64(stbl.Offset),
			idx.Name, "stbl missing stsz")
	}
	sttsP, sttsBI, err := payloadOf(f, stbl, mp4.BoxTypeStts())
	if err != nil {
		return err
	}
	if sttsP == nil {
		return model.NewFault(model.FaultMissingIndex, int64(stbl.Offset),
			idx.Name, "stbl missing stts")
	}
	stsc := stscP.(*mp4.Stsc)
	stsz := stszP.(*mp4.Stsz)
	stts := sttsP.(*mp4.Stts)

	chunkOff, nChunks, _, err := readChunkOffsets(f, stbl, idx)
	if err != nil {
		return err
	}
	if nChunks == 0 {
		return model.NewFault(model.FaultMissingIndex, int64(stbl.Offset),
			idx.Name, "empty chunk offset table")
	}

	// per-chunk sample counts from the run-length encoded stsc table
	samplesPerChunk := make([]uint32, nChunks)
	if len(stsc.Entries) == 0 || stsc.Entries[0].FirstChunk != 1 {
		return model.NewFault(model.FaultContainerBounds, int64(stscBI.Offset),
			idx.Name, "stsc first entry must start at chunk 1")
	}
	run := 0
	for c := uint32(0); c < nChunks; c++ {
		chunkNo := c + 1
		for run+1 < len(stsc.Entries) && stsc.Entries[run+1].FirstChunk <= chunkNo {
			run++
		}
		samplesPerChunk[c] = stsc.Entries[run].SamplesPerChunk
	}

	ctts := readCtts(f, stbl)
	keyframes := readKeyframes(f, stbl, idx)

	// expand samples chunk by chunk, validating every byte range
	var samples []Sample
	var sampleNo uint32
	mdatEnd := idx.MdatPayload + (idx.MdatSize - 8)
	if idx.MdatSize == 0 {
		mdatEnd = idx.Size
	}
	for c := uint32(0); c < nChunks; c++ {
		base := int64(chunkOff[c])
		cursor := base
		for k := uint32(0); k < samplesPerChunk[c]; k++ {
			size, ok := sampleSize(stsz, sampleNo)
			if !ok {
				addFault(model.NewFault(model.FaultMissingIndex,
					int64(stszBI.Offset), idx.Name,
					fmt.Sprintf("stsz has no entry for sample %d", sampleNo)))
				return nil
			}
			off := cursor
			if off < idx.MdatPayload || int64(size) < 0 ||
				off+int64(size) > mdatEnd {
				addFault(model.NewFault(model.FaultOutOfBounds, off, idx.Name,
					fmt.Sprintf("sample %d [%d,+%d) outside mdat payload [%d,%d)",
						sampleNo, off, size, idx.MdatPayload, mdatEnd)))
				return nil
			}
			samples = append(samples, Sample{Index: sampleNo, Offset: off,
				Size: size, IsKeyframe: keyframes[sampleNo]})
			cursor += int64(size)
			sampleNo++
		}
	}

	// DTS from stts, composition offsets from ctts. Native timescale units
	// are kept alongside nanoseconds so wrap/regression detection never
	// suffers a rounding round-trip.
	deltas := expandStts(stts)
	if len(deltas) != len(samples) {
		addFault(model.NewFault(model.FaultContainerBounds, int64(sttsBI.Offset),
			idx.Name, fmt.Sprintf("stts covers %d samples but %d located",
				len(deltas), len(samples))))
	}
	var dtsUnits uint64
	rawPTS := make([]int64, len(samples))
	for i := range samples {
		var delta uint32
		if i < len(deltas) {
			delta = deltas[i]
		}
		dtsUnits += uint64(delta)
		samples[i].DTS = model.ToNanoseconds(dtsUnits, tr.Timescale)
		var cOff int64
		if i < len(ctts) {
			cOff = ctts[i]
		}
		rawPTS[i] = int64(dtsUnits) + cOff
		samples[i].PTS = samples[i].DTS + signedNano(cOff, tr.Timescale)
	}
	detectPTSFaults(samples, rawPTS, tr, idx, addFault)
	tr.Samples = samples
	return nil
}
