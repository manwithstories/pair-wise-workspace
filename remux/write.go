// Package remux performs decode-free, lossless container remuxing to CMAF/fMP4.
package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"fieldpost/internal/core"
)

// FragmentInfo describes one keyframe-aligned movie fragment.
type FragmentInfo struct {
	Number     int
	StartIndex int
	Count      int
	BaseDTS    core.Ticks
	ByteOffset int
}

// Result contains a complete fragmented MP4 presentation and fragment map.
type Result struct {
	Data      []byte
	Fragments []FragmentInfo
	Essence   string
}

type trackData struct {
	info    trackInfo
	samples []core.Sample
	payload bytes.Buffer
}

type trunPatch struct {
	fieldPosInMoof int
	trackID        uint32
	payloadBytes   int
}

// CMAF builds ftyp+moov followed by one moof/mdat pair per keyframe group.
// Sample order, integer timestamps, durations, flags and essence bytes are
// retained.
func CMAF(tracks []core.Track, samples []core.Sample) (*Result, error) {
	if len(samples) == 0 {
		return nil, core.InvariantError(core.StageRemux, "cmaf", 0, "refusing to remux zero samples")
	}
	infos := make([]trackInfo, 0, len(tracks))
	videoIDs := map[uint32]bool{}
	for _, t := range tracks {
		kind := "vide"
		if t.Kind == core.TrackAudio {
			kind = "soun"
		} else {
			videoIDs[t.ID] = true
		}
		infos = append(infos, trackInfo{id: t.ID, kind: kind, codec: t.Codec, timescale: t.Timescale})
	}
	var head bytes.Buffer
	head.Write(ftypBox())
	head.Write(moovBox(infos))

	groups := keyframeGroups(samples, videoIDs)
	frags := make([]FragmentInfo, 0, len(groups))
	data := head.Bytes()
	for gi, idxRange := range groups {
		group := samples[idxRange[0]:idxRange[1]]
		fileOffset := len(data)
		chunk, err := fragmentBoxes(uint32(gi+1), tracks, group, fileOffset)
		if err != nil {
			return nil, err
		}
		frags = append(frags, FragmentInfo{
			Number: gi + 1, StartIndex: idxRange[0], Count: len(group),
			BaseDTS: group[0].DTS, ByteOffset: fileOffset,
		})
		data = append(data, chunk...)
	}
	return &Result{Data: data, Fragments: frags,
		Essence: core.EssenceHash(&core.Segment{Tracks: tracks, Samples: samples})}, nil
}

func keyframeGroups(samples []core.Sample, videoIDs map[uint32]bool) [][2]int {
	var groups [][2]int
	start := 0
	for i := 1; i < len(samples); i++ {
		if videoIDs[samples[i].TrackID] && samples[i].Flags&core.FlagSync != 0 {
			groups = append(groups, [2]int{start, i})
			start = i
		}
	}
	return append(groups, [2]int{start, len(samples)})
}

func fragmentBoxes(seq uint32, tracks []core.Track, group []core.Sample, fileOffset int) ([]byte, error) {
	byID := map[uint32]*trackData{}
	var order []uint32
	for _, tr := range tracks {
		kind := "vide"
		if tr.Kind == core.TrackAudio {
			kind = "soun"
		}
		td := &trackData{info: trackInfo{id: tr.ID, kind: kind, timescale: tr.Timescale}}
		byID[tr.ID] = td
		order = append(order, tr.ID)
	}
	for _, sm := range group {
		td := byID[sm.TrackID]
		if td == nil {
			return nil, core.InvariantError(core.StageRemux, "cmaf", int64(sm.TrackID),
				"sample references unmapped track %d", sm.TrackID)
		}
		td.samples = append(td.samples, sm)
		td.payload.Write(sm.Data)
	}

	var moofPayload bytes.Buffer
	var patches []trunPatch
	moofPayload.Write(fullBox("mfhd", 0, 0, u32(seq)))
	for _, id := range order {
		td := byID[id]
		if len(td.samples) == 0 {
			continue
		}
		var traf bytes.Buffer
		traf.Write(fullBox("tfhd", 0, 0x020000, u32(id)))
		traf.Write(fullBox("tfdt", 1, 0, u64(uint64(td.samples[0].DTS))))

		var trunPayload bytes.Buffer
		trunPayload.Write(u32(uint32(len(td.samples))))
		trunPayload.Write(u32(0)) // data_offset placeholder
		for _, sm := range td.samples {
			trunPayload.Write(u32(uint32(sm.Duration)))
			trunPayload.Write(u32(uint32(len(sm.Data))))
			trunPayload.Write(u32(encodeMP4Flags(sm.Flags)))
		}
		trunBoxStart := traf.Len()
		trunBox := fullBox("trun", 0, 0x000701, trunPayload.Bytes())
		traf.Write(trunBox)

		// moof header(8) + current moof payload + traf header(8) +
		// position of trun in traf payload + data_offset field in trun(16).
		fieldPos := 8 + moofPayload.Len() + 8 + trunBoxStart + 16
		patches = append(patches, trunPatch{
			fieldPosInMoof: fieldPos, trackID: id, payloadBytes: td.payload.Len(),
		})
		moofPayload.Write(box("traf", traf.Bytes()))
	}
	moof := box("moof", moofPayload.Bytes())
	mdatPayloadAbs := fileOffset + len(moof) + 8
	mdatCursor := 0
	for _, p := range patches {
		td := byID[p.trackID]
		value := uint32(mdatPayloadAbs + mdatCursor)
		pos := p.fieldPosInMoof
		binaryPutU32(moof[pos:pos+4], value)
		mdatCursor += p.payloadBytes
		_ = td
	}
	var mdatPayload bytes.Buffer
	for _, id := range order {
		if td := byID[id]; len(td.samples) > 0 {
			mdatPayload.Write(td.payload.Bytes())
		}
	}
	var out bytes.Buffer
	out.Write(moof)
	out.Write(box("mdat", mdatPayload.Bytes()))
	return out.Bytes(), nil
}

func encodeMP4Flags(f uint32) uint32 {
	out := f & 0x0000ffff
	if f&core.FlagSync == 0 {
		out |= 0x00010000 // sample_is_non_sync_sample
	}
	return out
}

// AtomicWrite writes data to a temporary file in dir and renames it only after
// close. A failed write removes the temporary file, so callers never observe a
// half-written target segment.
func AtomicWrite(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", core.Wrap(core.StageRemux, core.ExitUnrecoverable, true, err, "create output dir")
	}
	final := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".tmp-"+name+".*")
	if err != nil {
		return "", core.Wrap(core.StageRemux, core.ExitUnrecoverable, true, err, "create temp fragment")
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", core.Wrap(core.StageRemux, core.ExitUnrecoverable, true, err, "write fragment")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", core.Wrap(core.StageRemux, core.ExitUnrecoverable, true, err, "close fragment")
	}
	if err := os.Rename(tmpName, final); err != nil {
		cleanup()
		return "", core.Wrap(core.StageRemux, core.ExitUnrecoverable, true, err, "commit fragment")
	}
	return final, nil
}

// HashString returns a stable hex digest for arbitrary output bytes.
func HashString(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func binaryPutU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
