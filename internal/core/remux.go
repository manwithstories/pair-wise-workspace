package core

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"

	"mediabridge/internal/clock"
	"mediabridge/internal/fault"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
)

// OutputFormat identifies the delivery container layout. The kernel writes a
// simple, self-describing stream rather than re-muxing ISO BMFF: the goal is a
// verifiable copy of encoded packets with a stable hash, not a player-ready
// file. Verification reads it back and re-hashes every packet.
const OutputFormat = "MBRX1"

// OutputHeader describes the archive stream layout:
//
//	magic        [5]byte  "MBRX1"
//	assetLen     uint32   length of the asset id
//	asset        [assetLen]
//	trackCount   uint32
//	  per track:  slotLen u16, slot, trackID u32, kind u8,
//	              timescale u32, originNS i64, packetCount u64
//	packetCount  uint64
//	  per packet: slotLen u16, slot, trackID u32, packetIndex i64,
//	              dts i64, pts i64, dur i64, sync u8, size u32, payload
//
// All integers big-endian.
const (
	outputMagic   = OutputFormat
	outputVersion = 1
)

// RemuxInput is the fully demuxed, mapped asset ready for output.
type RemuxInput struct {
	Asset *media.Asset
	Plan  *maptrack.Plan
	// Reader supplies encoded bytes for a packet when Payload is nil.
	Reader SampleSource
	// Provenance is populated by the kernel as packets are copied.
	Provenance *Provenance
}

// SampleSource streams encoded bytes by (track id, byte offset, size).
type SampleSource interface {
	ReadSample(trackID uint32, offset, size int64) ([]byte, error)
}

// RemuxResult summarises a completed copy.
type RemuxResult struct {
	OutputBytes  int64
	OutputSHA256 string
	PacketCount  int64
	PerTrack     map[string]int64
}

// Remux copies every packet named by the plan into the output stream, hashing
// each payload and recording provenance. It never decodes.
func Remux(in RemuxInput, outPath string) (*RemuxResult, error) {
	if in.Asset == nil || in.Plan == nil {
		return nil, fault.New(fault.CodeMapping, fault.Location{}, "remux: missing asset or plan").
			WithOp("core.Remux")
	}
	byIndex := map[int]maptrack.Assignment{}
	for _, a := range in.Plan.Assignments {
		byIndex[a.TrackIndex] = a
	}

	// Expand the plan into a packet stream ordered by (slot order, pts, index).
	stream := buildStream(in.Asset, byIndex)

	f, err := os.CreateTemp(dirOf(outPath), ".mbx-*")
	if err != nil {
		return nil, fault.New(fault.CodeCheckpoint, fault.Location{AssetID: in.Asset.ID},
			"create temp output failed").Wrap(err).WithOp("core.Remux")
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	bw := bufio.NewWriterSize(mw, 256<<10)
	w := &countingWriter{w: bw}

	if err := writeOutputHeader(bw, in.Asset, in.Plan, int64(len(stream))); err != nil {
		f.Close()
		return nil, err
	}

	res := &RemuxResult{PerTrack: map[string]int64{}}
	outputIndex := map[string]int64{}
	for _, item := range stream {
		payload, err := loadPayload(in, item.pkt)
		if err != nil {
			f.Close()
			return nil, err
		}
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		item.pkt.SHA256 = digest
		if err := writePacket(bw, item.slot, item.pkt, payload); err != nil {
			f.Close()
			return nil, err
		}
		if in.Provenance != nil {
			idx := outputIndex[item.slot]
			rec := item.pkt
			rec.SHA256 = digest
			in.Provenance.AddPacket(item.slot, idx, rec)
			outputIndex[item.slot]++
		}
		res.PerTrack[item.slot]++
		res.PacketCount++
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return nil, fault.New(fault.CodeVerify, fault.Location{AssetID: in.Asset.ID},
			"flush output failed").Wrap(err).WithOp("core.Remux")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fault.New(fault.CodeVerify, fault.Location{AssetID: in.Asset.ID},
			"sync output failed").Wrap(err).WithOp("core.Remux")
	}
	if err := f.Close(); err != nil {
		return nil, fault.New(fault.CodeVerify, fault.Location{AssetID: in.Asset.ID},
			"close output failed").Wrap(err).WithOp("core.Remux")
	}

	// Atomic publish: rename the fully written temp file into place.
	if err := os.Rename(tmp, outPath); err != nil {
		return nil, fault.New(fault.CodeCheckpoint, fault.Location{AssetID: in.Asset.ID},
			"publish output failed").Wrap(err).WithOp("core.Remux")
	}
	tmp = ""

	res.OutputBytes = w.n
	res.OutputSHA256 = hex.EncodeToString(hasher.Sum(nil))
	if in.Provenance != nil {
		in.Provenance.OutputSHA256 = res.OutputSHA256
		in.Provenance.OutputBytes = res.OutputBytes
	}
	return res, nil
}

// streamItem is one packet destined for the output.
type streamItem struct {
	slot string
	pkt  media.Packet
}

// buildStream selects packets for each mapped track and orders them for the
// output. The order is deterministic: slots in plan order, packets by packet
// index.
func buildStream(asset *media.Asset, assignments map[int]maptrack.Assignment) []streamItem {
	var out []streamItem
	for _, a := range sortedAssignments(assignments) {
		for _, pkt := range asset.Packets {
			if pkt.TrackIndex != a.TrackIndex {
				continue
			}
			cp := pkt
			cp.SourceTrackID = a.TrackID
			out = append(out, streamItem{slot: a.Slot, pkt: cp})
		}
	}
	return out
}

func sortedAssignments(m map[int]maptrack.Assignment) []maptrack.Assignment {
	out := make([]maptrack.Assignment, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	// Stable plan order: video first, audio second, then aux by slot name.
	order := map[string]int{"video": 0, "audio": 1}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && lessAssignment(out[j], out[j-1], order); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func lessAssignment(a, b maptrack.Assignment, order map[string]int) bool {
	oa, oka := order[a.Slot]
	ob, okb := order[b.Slot]
	if oka != okb {
		return oka // known slots sort before auxiliary
	}
	if oka && ob != oa {
		return oa < ob
	}
	return a.Slot < b.Slot
}

func loadPayload(in RemuxInput, pkt media.Packet) ([]byte, error) {
	if len(pkt.Payload) > 0 {
		return pkt.Payload, nil
	}
	if in.Reader == nil {
		return nil, fault.New(fault.CodeVerify,
			fault.Location{AssetID: in.Asset.ID}.WithTrack(pkt.SourceTrackID).WithOffset(pkt.SourceOffset),
			"packet has no payload and no sample reader").WithOp("core.Remux")
	}
	data, err := in.Reader.ReadSample(pkt.SourceTrackID, pkt.SourceOffset, pkt.SourceSize)
	if err != nil {
		return nil, fault.New(fault.CodeVerify,
			fault.Location{AssetID: in.Asset.ID}.WithTrack(pkt.SourceTrackID).WithOffset(pkt.SourceOffset),
			"read sample failed").Wrap(err).WithOp("core.Remux")
	}
	if int64(len(data)) != pkt.SourceSize {
		return nil, fault.New(fault.CodeVerify,
			fault.Location{AssetID: in.Asset.ID}.WithTrack(pkt.SourceTrackID).WithOffset(pkt.SourceOffset),
			"short sample read: %d of %d", len(data), pkt.SourceSize).WithOp("core.Remux")
	}
	return data, nil
}

func dirOf(path string) string {
	dir := os.TempDir()
	if i := lastSlash(path); i >= 0 {
		dir = path[:i]
		if dir == "" {
			dir = "/"
		}
	}
	return dir
}

func lastSlash(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return i
		}
	}
	return -1
}

type countingWriter struct {
	w *bufio.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Flush drains the buffered writer into the file and hasher.
func (c *countingWriter) Flush() error { return c.w.Flush() }

var _ hash.Hash = sha256.New()
var _ = clock.NSPerSecond
