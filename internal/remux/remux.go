// Package remux repackages recoverable media onto the unified timeline.
// Output is written to a temporary file inside the user-specified
// directory and atomically renamed only after verification passes.
// Unrecoverable damage is reported with its raw offset and never
// silently skipped.
package remux

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// MaxPacketSize caps a single sample payload (16 MiB); memory grows with
// queue capacity times this limit, never with total file size.
const MaxPacketSize = 16 << 20

// TrackData is one track's worth of located packets for WriteContainer.
// Timescale is the track's native timescale; zero means the unified base.
type TrackData struct {
	ID        uint32
	MediaType string
	Timescale uint32
	Packets   []scan.Packet
}

// WriteContainer writes tracks as a self-contained MP4 to w, reading
// sample payloads from src. It is shared by the remux stage and the
// sample generator.
func WriteContainer(w io.Writer, src io.ReaderAt, tds []TrackData) error {
	var tracks []*trackOut
	for _, td := range tds {
		tracks = append(tracks, &trackOut{
			id:        td.ID,
			mediaType: td.MediaType,
			timescale: td.Timescale,
			pkts:      td.Packets,
		})
	}
	return writeMP4(w, src, tracks)
}

// Policy controls recovery behaviour.
type Policy struct {
	DropTruncatedTail bool // retain the intact prefix of a truncated track
	AssumeAllSync     bool // rebuild a missing keyframe index as all-sync
}

// Result summarizes one remux.
type Result struct {
	OutputPath        string
	RecoveredSegments int
	Packets           []scan.Packet // retained packets on the unified base
	Faults            fault.List
	tmpPath           string
}

// Remux writes the retained packets of all tracks to outDir on the unified
// 90 kHz base. The caller must call Finalize after verification, or
// Abandon to remove the temporary file.
func Remux(ctx context.Context, src io.ReaderAt, ci *scan.ContainerInfo,
	byTrack map[uint32][]scan.Packet, outDir string, pol Policy) (*Result, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fault.New(fault.KindWriteFailure, fault.StageWrite, 0, 0, -1, false,
			fmt.Sprintf("output dir %q not usable: %v", abs, err))
	}

	res := &Result{}
	var tracks []*trackOut
	for _, t := range ci.Tracks {
		pkts := byTrack[t.ID]
		if len(pkts) == 0 {
			continue
		}
		if t.MediaType == "video" && t.SyncSamples == nil && !pol.AssumeAllSync {
			res.Faults = append(res.Faults, fault.New(fault.KindMissingIndex, fault.StageRemux,
				t.ID, 0, -1, false, "missing keyframe index and policy forbids rebuild"))
			continue
		}
		tr := &trackOut{id: t.ID, mediaType: t.MediaType, pkts: pkts}
		tracks = append(tracks, tr)
		res.Packets = append(res.Packets, pkts...)
		res.RecoveredSegments++ // one contiguous retained run per track
	}
	if len(tracks) == 0 {
		res.Faults = append(res.Faults, fault.New(fault.KindUnrecoverable, fault.StageRemux,
			0, 0, ci.DamageOffset, false, "no recoverable track data"))
		return res, nil
	}

	tmp, err := os.CreateTemp(abs, ".aircheck-*.mp4")
	if err != nil {
		return nil, fault.New(fault.KindWriteFailure, fault.StageWrite, 0, 0, -1, false, err.Error())
	}
	res.tmpPath = tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(res.tmpPath)
		}
	}()

	if err := writeMP4(tmp, src, tracks); err != nil {
		return nil, fault.New(fault.KindWriteFailure, fault.StageWrite, 0, 0, -1, false, err.Error())
	}
	if err := tmp.Sync(); err != nil {
		return nil, fault.New(fault.KindWriteFailure, fault.StageWrite, 0, 0, -1, false, err.Error())
	}
	ok = true
	return res, nil
}

// Finalize atomically moves the verified output to its final path inside
// the same directory.
func (r *Result) Finalize(name string) error {
	if r.tmpPath == "" {
		return fmt.Errorf("nothing to finalize")
	}
	final := filepath.Join(filepath.Dir(r.tmpPath), filepath.Base(name))
	if err := os.Rename(r.tmpPath, final); err != nil {
		return fault.New(fault.KindWriteFailure, fault.StageWrite, 0, 0, -1, false, err.Error())
	}
	r.OutputPath = final
	r.tmpPath = ""
	return nil
}

// Abandon removes the unverified temporary output.
func (r *Result) Abandon() {
	if r.tmpPath != "" {
		os.Remove(r.tmpPath)
		r.tmpPath = ""
	}
}

// --- minimal deterministic MP4 writer on the unified 90 kHz base ---

type trackOut struct {
	id        uint32
	mediaType string
	timescale uint32 // 0 = unified base
	pkts      []scan.Packet
	dataOff   uint64 // offset of this track's data inside mdat payload
	dataLen   uint64
}

func be32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	be32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullbox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := make([]byte, 4)
	head[0] = version
	head[1] = byte(flags >> 16)
	head[2] = byte(flags >> 8)
	head[3] = byte(flags)
	return box(typ, append(head, payload...))
}

func u32(v uint32) []byte { b := make([]byte, 4); be32(b, v); return b }

func writeMP4(w io.Writer, src io.ReaderAt, tracks []*trackOut) error {
	// Lay out track data inside mdat and compute chunk offsets.
	var mdatLen uint64
	for _, tr := range tracks {
		tr.dataOff = mdatLen
		for _, p := range tr.pkts {
			if p.Size > MaxPacketSize {
				return fmt.Errorf("sample %d of track %d exceeds %d bytes", p.Index, tr.id, MaxPacketSize)
			}
			tr.dataLen += uint64(p.Size)
		}
		mdatLen += tr.dataLen
	}

	ftyp := box("ftyp", append(append([]byte("isom"), u32(0)...), []byte("isomiso2")...))
	moov := buildMoov(tracks)
	mdatHead := make([]byte, 8)
	be32(mdatHead[0:4], uint32(8+mdatLen))
	copy(mdatHead[4:8], "mdat")

	// Patch stco now that the moov length is known: data starts after
	// ftyp + moov + mdat header.
	base := uint64(len(ftyp) + len(moov) + 8)
	for _, tr := range tracks {
		tr.dataOff += base
	}
	moov = buildMoov(tracks) // rebuild with final offsets (length unchanged)

	if _, err := w.Write(ftyp); err != nil {
		return err
	}
	if _, err := w.Write(moov); err != nil {
		return err
	}
	if _, err := w.Write(mdatHead); err != nil {
		return err
	}
	buf := make([]byte, MaxPacketSize)
	for _, tr := range tracks {
		for _, p := range tr.pkts {
			if _, err := src.ReadAt(buf[:p.Size], p.Offset); err != nil {
				return fmt.Errorf("read sample %d of track %d at %d: %w", p.Index, tr.id, p.Offset, err)
			}
			if _, err := w.Write(buf[:p.Size]); err != nil {
				return err
			}
		}
	}
	return nil
}
