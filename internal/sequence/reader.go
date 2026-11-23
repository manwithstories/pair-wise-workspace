// Package sequence implements the on-disk medpipe frame sequence format:
// a header of track declarations followed by integrity-checked frame
// records. Every record carries its own SHA-256 so a damaged payload is
// always attributed to a track, frame number and byte offset.
package sequence

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"medpipe/internal/model"
)

var magic = [8]byte{'M', 'E', 'D', 'P', 'S', 'E', 'Q', 1}

const (
	maxHeaderBytes  = 1 << 20
	maxPayloadBytes = 8 << 20
	maxFrames       = 1 << 20
	maxMetaEntries  = 64
	maxStringBytes  = 4096
)

// Header is the JSON portion of the sequence file.
type Header struct {
	Version    uint32        `json:"version"`
	Tracks     []model.Track `json:"tracks"`
	FrameCount uint32        `json:"frame_count"`
}

// File is a fully validated sequence.
type File struct {
	Header  Header
	Frames  map[string][]model.Frame
	Digest  [32]byte // SHA-256 of the entire byte stream
	Size    int64
	Offsets []int64 // payload offsets for each frame, parallel to Frames
}

// ResolveWithin joins rel onto base and rejects absolute paths, traversal
// outside base and any attempt to escape through a symlink target spelling.
func ResolveWithin(base, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", model.New(model.KindInput, "sequence.ResolveWithin", fmt.Sprintf("path %q escapes base directory", rel))
	}
	joined := filepath.Join(base, clean)
	back, err := filepath.Rel(base, joined)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", model.New(model.KindInput, "sequence.ResolveWithin", fmt.Sprintf("path %q escapes base directory", rel))
	}
	return joined, nil
}

// ReadFile reads and validates a sequence file located inside root. A root of
// "" disables containment checking (used for absolute temp paths internally).
func ReadFile(root, rel string) (*File, error) {
	path := rel
	if root != "" {
		p, err := ResolveWithin(root, rel)
		if err != nil {
			return nil, err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.Wrap(model.KindInput, "sequence.ReadFile", err)
	}
	return Parse(data)
}

// Parse validates a byte stream. It never skips a damaged frame: the first
// checksum or structural violation aborts with a KindCorrupt error carrying
// the track, frame and offset.
func Parse(data []byte) (*File, error) {
	sum := sha256.Sum256(data)
	offset := int64(0)
	if len(data) < len(magic)+4 {
		return nil, model.Corruption("sequence.Parse", "", 0, 0, "file shorter than sequence header")
	}
	if !bytes.Equal(data[:8], magic[:]) {
		return nil, model.Corruption("sequence.Parse", "", 0, 0, "bad magic")
	}
	offset = 8
	hlen := binary.BigEndian.Uint32(data[offset:])
	offset += 4
	if hlen > maxHeaderBytes || int64(hlen) > int64(len(data))-offset {
		return nil, model.Corruption("sequence.Parse", "", 0, offset, "invalid header length")
	}
	var h Header
	dec := json.NewDecoder(bytes.NewReader(data[offset : offset+int64(hlen)]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return nil, model.Corruption("sequence.Parse", "", 0, offset, "header decode: "+err.Error())
	}
	offset += int64(hlen)
	if h.Version != 1 {
		return nil, model.New(model.KindInput, "sequence.Parse", fmt.Sprintf("unsupported sequence version %d", h.Version))
	}
	if len(h.Tracks) == 0 {
		return nil, model.Corruption("sequence.Parse", "", 0, offset, "header declares no tracks")
	}
	if h.FrameCount > maxFrames {
		return nil, model.Corruption("sequence.Parse", "", 0, offset, "frame count exceeds limit")
	}
	trackByID := make(map[string]int, len(h.Tracks))
	for i, t := range h.Tracks {
		if t.ID == "" || t.Timescale == 0 {
			return nil, model.Corruption("sequence.Parse", t.ID, 0, offset, "track has empty ID or zero timescale")
		}
		if _, dup := trackByID[t.ID]; dup {
			return nil, model.Corruption("sequence.Parse", t.ID, 0, offset, "duplicate track ID")
		}
		trackByID[t.ID] = i
	}

	f := &File{Header: h, Frames: make(map[string][]model.Frame, len(h.Tracks)), Digest: sum, Size: int64(len(data))}
	var total uint32
	var offsets []int64
	for offset < int64(len(data)) {
		recOff := offset
		if int64(len(data))-offset < 4+8+8+1+2+2+2+8 {
			return nil, model.Corruption("sequence.Parse", "", uint64(total), recOff, "truncated frame record header")
		}
		tIdx := binary.BigEndian.Uint32(data[offset:])
		offset += 4
		seq := binary.BigEndian.Uint64(data[offset:])
		offset += 8
		rawTS := binary.BigEndian.Uint64(data[offset:])
		offset += 8
		flags := data[offset]
		offset++
		width := binary.BigEndian.Uint16(data[offset:])
		offset += 2
		height := binary.BigEndian.Uint16(data[offset:])
		offset += 2
		energyN := binary.BigEndian.Uint16(data[offset:])
		offset += 2
		plen := binary.BigEndian.Uint64(data[offset:])
		offset += 8

		if int(tIdx) >= len(h.Tracks) {
			return nil, model.Corruption("sequence.Parse", "", seq, recOff, "track index out of range")
		}
		track := h.Tracks[tIdx]
		if energyN > 4096 {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "energy sample count exceeds limit")
		}
		if plen > maxPayloadBytes {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "payload length exceeds limit")
		}
		if plen > uint64(int64(len(data))-offset) {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "payload runs past end of file")
		}
		payload := append([]byte(nil), data[offset:offset+int64(plen)]...)
		offset += int64(plen)
		energy := make([]float64, energyN)
		need := int64(energyN) * 8
		if need > int64(len(data))-offset {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "energy samples run past end of file")
		}
		for i := range energy {
			energy[i] = float64FromBits(binary.BigEndian.Uint64(data[offset:]))
			offset += 8
		}
		if int64(len(data))-offset < 2 {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "missing metadata count")
		}
		metaN := binary.BigEndian.Uint16(data[offset:])
		offset += 2
		if int(metaN) > maxMetaEntries {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "metadata entry count exceeds limit")
		}
		meta := make([]model.Metadata, 0, metaN)
		for i := 0; i < int(metaN); i++ {
			if int64(len(data))-offset < 4 {
				return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "truncated metadata entry")
			}
			klen := int(binary.BigEndian.Uint16(data[offset:]))
			vlen := int(binary.BigEndian.Uint16(data[offset+2:]))
			offset += 4
			if klen == 0 || klen > maxStringBytes || vlen > maxStringBytes || int64(klen+vlen) > int64(len(data))-offset {
				return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "invalid metadata string length")
			}
			k := string(data[offset : offset+int64(klen)])
			offset += int64(klen)
			v := string(data[offset : offset+int64(vlen)])
			offset += int64(vlen)
			meta = append(meta, model.Metadata{Key: k, Value: v})
		}
		if int64(len(data))-offset < 32 {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "missing checksum")
		}
		var stored [32]byte
		copy(stored[:], data[offset:offset+32])
		offset += 32

		fr := model.Frame{
			TrackID: track.ID, Seq: seq, RawTS: rawTS, Keyframe: flags&1 != 0,
			Width: width, Height: height, Energy: energy, Payload: payload,
			Checksum: stored, Meta: meta, Offset: recOff,
		}
		want := fr.IntegrityChecksum()
		if stored != want {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "checksum mismatch")
		}
		prev := f.Frames[track.ID]
		if len(prev) > 0 && seq <= prev[len(prev)-1].Seq {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "frame sequence not strictly increasing")
		}
		f.Frames[track.ID] = append(prev, fr)
		offsets = append(offsets, recOff)
		total++
		if total > h.FrameCount {
			return nil, model.Corruption("sequence.Parse", track.ID, seq, recOff, "more frames than header declares")
		}
	}
	if total != h.FrameCount {
		return nil, model.Corruption("sequence.Parse", "", uint64(total), offset, fmt.Sprintf("header declares %d frames, file has %d", h.FrameCount, total))
	}
	f.Offsets = offsets
	return f, nil
}

// DigestFrames computes a canonical digest over frame identity and payload in
// track/sequence order. It is independent of metadata and file layout, which
// makes it useful for round-trip verification.
func DigestFrames(tracks []model.Track, frames map[string][]model.Frame) [32]byte {
	h := sha256.New()
	var b [8]byte
	for _, tr := range tracks {
		h.Write([]byte(tr.ID))
		h.Write([]byte{0})
		for _, fr := range frames[tr.ID] {
			binary.BigEndian.PutUint64(b[:], fr.Seq)
			h.Write(b[:])
			binary.BigEndian.PutUint64(b[:], fr.RawTS)
			h.Write(b[:])
			d := fr.ContentDigest()
			h.Write(d[:])
			binary.BigEndian.PutUint64(b[:], uint64(len(fr.Energy)))
			h.Write(b[:])
			for _, e := range fr.Energy {
				binary.BigEndian.PutUint64(b[:], float64Bits(e))
				h.Write(b[:])
			}
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func WriteTo(w io.Writer, tracks []model.Track, frames map[string][]model.Frame) (int64, [32]byte, error) {
	hw := &hashingWriter{w: w, h: sha256.New()}
	if err := writeSequence(hw, tracks, frames); err != nil {
		return hw.n, hw.sum(), err
	}
	return hw.n, hw.sum(), nil
}

type hashingWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (hw *hashingWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	if n > 0 {
		hw.h.Write(p[:n])
		hw.n += int64(n)
	}
	return n, err
}

func (hw *hashingWriter) sum() [32]byte {
	var out [32]byte
	copy(out[:], hw.h.Sum(nil))
	return out
}
