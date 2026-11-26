package wavscan

import (
	"io"
	"os"
	"strings"

	"archivectl/internal/core"
)

// Report is the structural result of a streaming RIFF/WAVE scan.
type Report struct {
	Path       string      `json:"path"`
	FileSize   int64       `json:"file_size"`
	RIFFSize   uint32      `json:"riff_size"`
	Format     Format      `json:"format"`
	HasFmt     bool        `json:"has_fmt"`
	Chunks     []ChunkInfo `json:"chunks"`
	Cues       []CuePoint  `json:"cues,omitempty"`
	Data       *ChunkInfo  `json:"data,omitempty"`
	DataOffset int64       `json:"data_offset"`

	DeclaredDataBytes  int64 `json:"declared_data_bytes"`
	AvailableDataBytes int64 `json:"available_data_bytes"`
	DeclaredFrames     int64 `json:"declared_frames"`
	AvailableFrames    int64 `json:"available_frames"`
}

// ScanPath opens path and scans its RIFF structure.
func ScanPath(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, core.Wrap(core.KindIO, "wavscan.ScanPath", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, core.Wrap(core.KindIO, "wavscan.ScanPath", path, err)
	}
	r, err := Scan(f, st.Size())
	if err != nil {
		if ce, ok := err.(*core.Error); ok && ce.Path == "" {
			ce.Path = path
		}
		return nil, err
	}
	r.Path = path
	return r, nil
}

// Scan parses RIFF/WAVE structure from r. It never reads unknown chunk
// bodies and never executes or interprets their contents.
func Scan(r io.ReaderAt, size int64) (*Report, error) {
	const op = "wavscan.Scan"
	if size < 12 {
		return nil, core.New(core.KindMalformedFMT, op, "file shorter than RIFF header")
	}
	var head [12]byte
	if err := readFullAt(r, 0, head[:], op); err != nil {
		return nil, err
	}
	if string(head[0:4]) != IDRIFF {
		return nil, core.New(core.KindMalformedFMT, op, "missing RIFF magic")
	}
	if string(head[8:12]) != IDWAVE {
		return nil, core.New(core.KindMalformedFMT, op, "missing WAVE magic")
	}
	rep := &Report{RIFFSize: uint32(head[4]) | uint32(head[5])<<8 | uint32(head[6])<<16 | uint32(head[7])<<24}

	off := int64(12)
	for off < size {
		if size-off < 8 {
			return nil, core.Newf(core.KindChunkLength, op,
				"truncated chunk header at offset %d (%d trailing bytes)", off, size-off)
		}
		var hdr [8]byte
		if err := readFullAt(r, off, hdr[:], op); err != nil {
			return nil, err
		}
		id := fourCC(hdr[0:4])
		chunkSize := int64(uint32(hdr[4]) | uint32(hdr[5])<<8 | uint32(hdr[6])<<16 | uint32(hdr[7])<<24)
		dataOff := off + 8
		end := dataOff + chunkSize
		info := ChunkInfo{
			Index:        len(rep.Chunks),
			ID:           id,
			HeaderOffset: off,
			DataOffset:   dataOff,
			Size:         chunkSize,
			OddPadding:   chunkSize%2 == 1,
			Unknown:      id != IDFmt && id != IDData && id != IDCue,
		}
		if end > size {
			info.Truncated = true
			if id != IDData {
				rep.Chunks = append(rep.Chunks, info)
				available := size - dataOff
				if available < 0 {
					available = 0
				}
				if err := core.CheckAllocation(op, available); err != nil {
					return nil, err
				}
				body := make([]byte, available)
				if err := readFullAt(r, dataOff, body, op); err != nil {
					return nil, err
				}
				if id == IDFmt {
					if _, err := decodeFmt(body); err != nil {
						return nil, err
					}
				} else if id == IDCue {
					if _, err := decodeCue(body, chunkSize); err != nil {
						return nil, err
					}
				}
				return nil, core.Newf(core.KindChunkLength, op,
					"chunk %q at %d declares %d bytes but only %d remain", id, off, chunkSize, size-off-8)
			}
			rep.Chunks = append(rep.Chunks, info)
			rep.Data = &info
			rep.DataOffset = dataOff
			break
		}

		switch id {
		case IDFmt:
			if err := core.CheckAllocation(op, chunkSize); err != nil {
				return nil, err
			}
			body := make([]byte, chunkSize)
			if err := readFullAt(r, dataOff, body, op); err != nil {
				return nil, err
			}
			f, err := decodeFmt(body)
			if err != nil {
				return nil, err
			}
			rep.Format = f
			rep.HasFmt = true
		case IDCue:
			if err := core.CheckAllocation(op, chunkSize); err != nil {
				return nil, err
			}
			body := make([]byte, chunkSize)
			if err := readFullAt(r, dataOff, body, op); err != nil {
				return nil, err
			}
			cues, err := decodeCue(body, chunkSize)
			if err != nil {
				return nil, err
			}
			rep.Cues = cues
		case IDData:
			if rep.Data == nil {
				dataCopy := info
				rep.Data = &dataCopy
				rep.DataOffset = dataOff
			}
		}

		if chunkSize%2 == 1 {
			missing, err := checkOddPadding(r, id, end, size, op)
			if err != nil {
				return nil, err
			}
			info.PaddingPresent = !missing
			off = end + 1
		} else {
			off = end
		}
		rep.Chunks = append(rep.Chunks, info)
	}

	if !rep.HasFmt {
		return nil, core.New(core.KindMalformedFMT, op, "fmt chunk not found")
	}
	if rep.Data == nil {
		return nil, core.New(core.KindChunkLength, op, "data chunk not found")
	}
	rep.DeclaredDataBytes = rep.Data.Size
	if rep.Data.Truncated {
		rep.AvailableDataBytes = maxInt64(0, size-rep.DataOffset)
	} else {
		rep.AvailableDataBytes = rep.Data.Size
	}
	if rem := rep.DeclaredDataBytes % int64(rep.Format.BlockAlign); rem != 0 {
		return nil, core.Newf(core.KindAlignment, op,
			"data size %d is not aligned to frame size %d (remainder %d)",
			rep.DeclaredDataBytes, rep.Format.BlockAlign, rem)
	}
	rep.DeclaredFrames = rep.DeclaredDataBytes / int64(rep.Format.BlockAlign)
	rep.AvailableFrames = rep.AvailableDataBytes / int64(rep.Format.BlockAlign)
	return rep, nil
}

func checkOddPadding(r io.ReaderAt, chunkID string, bodyEnd, size int64, op string) (bool, error) {
	if bodyEnd >= size {
		return false, core.Newf(core.KindPadding, op,
			"odd-sized chunk %q ends at EOF without required pad byte", chunkID)
	}
	paddedOK := headerLooksValid(r, bodyEnd+1, size)
	unpaddedOK := headerLooksValid(r, bodyEnd, size)
	if !paddedOK && unpaddedOK {
		return true, core.Newf(core.KindPadding, op,
			"odd-sized chunk %q at %d is missing its pad byte", chunkID, bodyEnd-8)
	}
	var pad [1]byte
	_ = readFullAt(r, bodyEnd, pad[:], op)
	return false, nil
}

func headerLooksValid(r io.ReaderAt, off, size int64) bool {
	if off < 0 || off+8 > size {
		return false
	}
	var h [8]byte
	if _, err := r.ReadAt(h[:], off); err != nil {
		return false
	}
	id := string(h[0:4])
	if strings.TrimSpace(id) == "" {
		return false
	}
	for _, c := range h[0:4] {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	n := int64(uint32(h[4]) | uint32(h[5])<<8 | uint32(h[6])<<16 | uint32(h[7])<<24)
	return off+8+n <= size
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
