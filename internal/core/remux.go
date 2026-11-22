package core

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"mediabridge/internal/fault"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
)

// OutputFormat identifies the delivery container layout.
const OutputFormat = "MBRX1"

const outputVersion = 1

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
// each payload and recording provenance. It never decodes. The output is
// written to a temp file and atomically renamed into place.
func Remux(in RemuxInput, outPath string) (*RemuxResult, error) {
	if in.Asset == nil || in.Plan == nil {
		return nil, fault.New(fault.CodeMapping, fault.Location{}, "remux: missing asset or plan").
			WithOp("core.Remux")
	}
	byIndex := map[int]maptrack.Assignment{}
	for _, a := range in.Plan.Assignments {
		byIndex[a.TrackIndex] = a
	}
	stream := buildStream(in.Asset, byIndex)

	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".mbx-*")
	if err != nil {
		return nil, fault.New(fault.CodeCheckpoint, fault.Location{AssetID: in.Asset.ID},
			"create temp output failed").Wrap(err).WithOp("core.Remux")
	}
	tmpName := tmp.Name()
	tmp.Close()
	os.Remove(tmpName)
	defer os.Remove(tmpName)

	w, err := NewWriter(tmpName, 0, nil)
	if err != nil {
		return nil, fault.New(fault.CodeCheckpoint, fault.Location{AssetID: in.Asset.ID},
			"open temp output failed").Wrap(err).WithOp("core.Remux")
	}
	if err := w.WriteHeader(in.Asset, in.Plan, int64(len(stream))); err != nil {
		w.Close()
		return nil, err
	}

	res := &RemuxResult{PerTrack: map[string]int64{}}
	outputIndex := map[string]int64{}
	for _, item := range stream {
		payload, err := loadPayload(in, item.Pkt)
		if err != nil {
			w.Close()
			return nil, err
		}
		sum := sha256.Sum256(payload)
		item.Pkt.SHA256 = hex.EncodeToString(sum[:])
		if err := w.WritePacket(item.Slot, item.Pkt, payload); err != nil {
			w.Close()
			return nil, err
		}
		if in.Provenance != nil {
			in.Provenance.AddPacket(item.Slot, outputIndex[item.Slot], item.Pkt)
			outputIndex[item.Slot]++
		}
		res.PerTrack[item.Slot]++
		res.PacketCount++
	}
	if err := w.Sync(); err != nil {
		w.Close()
		return nil, fault.New(fault.CodeVerify, fault.Location{AssetID: in.Asset.ID},
			"flush output failed").Wrap(err).WithOp("core.Remux")
	}
	size, _ := w.Size()
	digest := w.Digest()
	if err := w.Close(); err != nil {
		return nil, fault.New(fault.CodeVerify, fault.Location{AssetID: in.Asset.ID},
			"close output failed").Wrap(err).WithOp("core.Remux")
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return nil, fault.New(fault.CodeCheckpoint, fault.Location{AssetID: in.Asset.ID},
			"publish output failed").Wrap(err).WithOp("core.Remux")
	}
	res.OutputBytes = size
	res.OutputSHA256 = digest
	if in.Provenance != nil {
		in.Provenance.OutputSHA256 = digest
		in.Provenance.OutputBytes = size
	}
	return res, nil
}

// StreamItem is one packet destined for the output.
type StreamItem struct {
	Slot string
	Pkt  media.Packet
}

// BuildStream selects packets for each mapped track and orders them
// deterministically: slots in delivery order, packets by packet index.
func BuildStream(asset *media.Asset, plan *maptrack.Plan) []StreamItem {
	byIndex := map[int]maptrack.Assignment{}
	for _, a := range plan.Assignments {
		byIndex[a.TrackIndex] = a
	}
	return buildStream(asset, byIndex)
}

func buildStream(asset *media.Asset, assignments map[int]maptrack.Assignment) []StreamItem {
	var out []StreamItem
	for _, a := range assignmentsSlice(assignments) {
		for _, pkt := range asset.Packets {
			if pkt.TrackIndex != a.TrackIndex {
				continue
			}
			cp := pkt
			cp.SourceTrackID = a.TrackID
			out = append(out, StreamItem{Slot: a.Slot, Pkt: cp})
		}
	}
	return out
}

// assignmentsSlice returns assignments in delivery order.
func assignmentsSlice(m map[int]maptrack.Assignment) []maptrack.Assignment {
	out := make([]maptrack.Assignment, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := slotRank(out[i].Slot), slotRank(out[j].Slot)
		if ri != rj {
			return ri < rj
		}
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		return out[i].TrackIndex < out[j].TrackIndex
	})
	return out
}

// OrderedAssignments returns a copy of assignments in delivery order.
func OrderedAssignments(as []maptrack.Assignment) []maptrack.Assignment {
	out := append([]maptrack.Assignment(nil), as...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := slotRank(out[i].Slot), slotRank(out[j].Slot)
		if ri != rj {
			return ri < rj
		}
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		return out[i].TrackIndex < out[j].TrackIndex
	})
	return out
}

func slotRank(slot string) int {
	switch slot {
	case "video":
		return 0
	case "audio":
		return 1
	default:
		return 2
	}
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

func sha256New() hashLike { return sha256.New() }

func hexEncode(b []byte) string { return hex.EncodeToString(b) }
