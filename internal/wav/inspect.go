package wav

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"mediaaudit/internal/model"
)

// TileFrames is the mandated per-CRC32C block size in sample frames.
const TileFrames = 32

// CRCChunkID is the custom sub-chunk holding one CRC32C per 32-frame tile.
// Payload: little-endian uint32 values, one for each full tile in order.
const CRCChunkID = "crCC"

// crcTable is the Castagnoli (CRC32C) polynomial table.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// TileResult reports one inspected tile.
type TileResult struct {
	Index       int64
	Range       model.FrameRange
	ByteOffset  int64
	Size        int64
	OK          bool
	WantCRC     uint32
	GotCRC      uint32
	Residual    bool // true for a short final tile with fewer than TileFrames
}

// InspectReport aggregates tile results and located faults.
type InspectReport struct {
	Tiles  []TileResult
	Faults *model.FaultSet
}

// CRCTable extracts the per-tile CRC32C list from a crCC chunk if present.
func (rd *Reader) CRCTable() ([]uint32, bool, error) {
	for _, c := range rd.chunks {
		if c.ID != CRCChunkID {
			continue
		}
		if c.Size%4 != 0 {
			return nil, false, model.NewFault(model.KindAlignment, "",
				model.FrameRange{}, c.Offset, "crCC size not a multiple of 4")
		}
		if c.Size > MaxDataBytes {
			return nil, false, model.NewFault(model.KindInvalid, "",
				model.FrameRange{}, c.Offset, "crCC payload exceeds limit")
		}
		buf := make([]byte, c.Size)
		if _, err := rd.r.Seek(c.Offset, io.SeekStart); err != nil {
			return nil, false, err
		}
		if _, err := io.ReadFull(rd.r, buf); err != nil {
			return nil, false, err
		}
		out := make([]uint32, c.Size/4)
		for i := range out {
			out[i] = binary.LittleEndian.Uint32(buf[i*4:])
		}
		return out, true, nil
	}
	return nil, false, nil
}

// Inspect streams PCM in 32-frame tiles, recomputing CRC32C and isolating a
// short trailing residual instead of condemning the whole track. Reading a
// tile whose bytes are absent from the file yields a truncated fault limited
// to that residual block.
func (rd *Reader) Inspect() (*InspectReport, error) {
	h := rd.Header()
	crcs, hasCRC, err := rd.CRCTable()
	if err != nil {
		return nil, err
	}
	rep := &InspectReport{Faults: model.NewFaultSet()}
	if rd.tailFault != nil {
		// Surface the recoverable short-tail fault as located damage while the
		// intact prefix still gets its full tile inspection below.
		rep.Faults.Add(*rd.tailFault)
	}
	if err := rd.SeekData(); err != nil {
		return nil, err
	}
	tileBytes := int64(TileFrames) * h.FrameBytes
	tileBuf := make([]byte, tileBytes)
	tileCount := h.FrameCount / TileFrames
	var tile int64
	for tile = 0; tile < tileCount; tile++ {
		off := h.DataChunk.Offset + tile*tileBytes
		n, readErr := io.ReadFull(rd.r, tileBuf)
		if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
			rep.addTruncated(tile, off, int64(n))
			return rep, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		tr := TileResult{
			Index:      tile,
			Range:      model.FrameRange{First: model.Frame(tile * TileFrames), Last: model.Frame((tile + 1) * TileFrames)},
			ByteOffset: off,
			Size:       tileBytes,
		}
		tr.GotCRC = crc32.Checksum(tileBuf, crcTable)
		if hasCRC && tile < int64(len(crcs)) {
			tr.WantCRC = crcs[tile]
			tr.OK = tr.GotCRC == tr.WantCRC
			if !tr.OK {
				rep.Faults.Add(model.Fault{
					Kind:       model.KindCRC,
					Track:      "",
					Range:      tr.Range,
					ByteOffset: off,
					Message: fmt.Sprintf("CRC32C mismatch: want %08x got %08x",
						tr.WantCRC, tr.GotCRC),
				})
			}
		} else {
			tr.OK = true // no reference CRC: structural pass only
		}
		rep.Tiles = append(rep.Tiles, tr)
	}
	// Residual frames that do not fill a whole tile.
	rem := h.FrameCount % TileFrames
	if rem > 0 {
		off := h.DataChunk.Offset + tile*tileBytes
		buf := make([]byte, rem*h.FrameBytes)
		n, readErr := io.ReadFull(rd.r, buf)
		tr := TileResult{
			Index:      tile,
			Range:      model.FrameRange{First: model.Frame(tile * TileFrames), Last: model.Frame(tile*TileFrames + rem)},
			ByteOffset: off,
			Size:       int64(n),
			Residual:   true,
			OK:         true,
		}
		if readErr != nil {
			tr.OK = false
			rep.Faults.Add(model.Fault{
				Kind:       model.KindTruncated,
				Range:      tr.Range,
				ByteOffset: off,
				Message:    "short trailing residual block: " + readErr.Error(),
			})
		}
		rep.Tiles = append(rep.Tiles, tr)
	}
	return rep, nil
}

func (r *InspectReport) addTruncated(tile, off, got int64) {
	r.Faults.Add(model.Fault{
		Kind:       model.KindTruncated,
		Range:      model.FrameRange{First: model.Frame(tile * TileFrames)},
		ByteOffset: off + got,
		Message:    fmt.Sprintf("tile %d truncated after %d bytes", tile, got),
	})
}

// DamagedRanges returns contiguous frame ranges covered by CRC/alignment
// faults, in frame order, so callers can map damage across tracks.
func (r *InspectReport) DamagedRanges() []model.FrameRange {
	var out []model.FrameRange
	for _, f := range r.Faults.All() {
		if f.Kind != model.KindCRC && f.Kind != model.KindAlignment && f.Kind != model.KindTruncated {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Last == f.Range.First {
			out[n-1].Last = f.Range.Last
			continue
		}
		out = append(out, f.Range)
	}
	return out
}
