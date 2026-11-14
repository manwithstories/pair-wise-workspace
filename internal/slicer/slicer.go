package slicer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tsprobe/internal/demux"
	"tsprobe/internal/psi"
	"tsprobe/internal/tspacket"
)

var (
	ErrNoSlices = errors.New("slicer: no keyframe slices produced")
	ErrNoTables = errors.New("slicer: PAT/PMT unavailable at first keyframe")
)

type PMTEntry struct {
	Program uint16 `json:"program"`
	Version uint8  `json:"version"`
}

type SliceInfo struct {
	File         string     `json:"file"`
	PacketOffset int64      `json:"packet_offset"`
	PacketCount  int        `json:"packet_count"`
	ByteLength   int64      `json:"byte_length"`
	StartPTS     int64      `json:"start_pts"`
	DurationMS   int64      `json:"duration_ms"`
	PATVersion   uint8      `json:"pat_version"`
	PMTVersions  []PMTEntry `json:"pmt_versions"`

	hasPTS     bool
	psiPackets int
}

type Index struct {
	Slices []SliceInfo `json:"slices"`
}

type clipWriter struct {
	file       *os.File
	info       SliceInfo
	startPTS   int64
	hasPTS     bool
	packetSize int64
}

// Slice performs a streaming split at video keyframes. PAT/PMT packets are
// filtered and replaced by the table snapshot effective at each cut point.
func Slice(inPath, outDir, indexPath string, minDuration time.Duration) ([]SliceInfo, error) {
	if minDuration <= 0 {
		return nil, fmt.Errorf("slicer: min-duration must be positive")
	}
	in, err := os.Open(inPath)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := removeStaleClips(outDir); err != nil {
		return nil, err
	}

	reader := tspacket.NewReader(in, 8)
	store := psi.NewStore()
	dmx := demux.New(store)
	var current *clipWriter
	var slices []SliceInfo
	var sliceNumber int

	for {
		packet, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		keyframe, err := dmx.Process(packet)
		if err != nil {
			return nil, err
		}
		if keyframe != nil {
			if current != nil && keyframe.HasPTS && current.hasPTS {
				elapsed := time.Duration(keyframe.PTS-current.startPTS) * time.Second / 90000
				if elapsed >= minDuration {
					if err := finishClip(current, keyframe.PTS, keyframe.HasPTS); err != nil {
						return nil, err
					}
					slices = append(slices, current.info)
					current = nil
				}
			}
			if current == nil {
				current, err = startClip(store, outDir, sliceNumber, keyframe)
				if err != nil {
					return nil, err
				}
				sliceNumber++
			}
		}
		if current != nil {
			if err := current.write(packet, store); err != nil {
				_ = current.file.Close()
				return nil, err
			}
		}
	}
	if current != nil {
		endPTS, hasPTS := dmx.LastVideoPTS()
		if !hasPTS || endPTS < current.startPTS {
			endPTS, hasPTS = current.startPTS, current.hasPTS
		}
		if err := finishClip(current, endPTS, hasPTS); err != nil {
			return nil, err
		}
		slices = append(slices, current.info)
	}
	if len(slices) == 0 {
		return nil, ErrNoSlices
	}
	if len(slices) >= 2 {
		last := slices[len(slices)-1]
		if time.Duration(last.DurationMS)*time.Millisecond < minDuration {
			if err := mergeTail(outDir, &slices[len(slices)-2], last); err != nil {
				return nil, err
			}
			slices = slices[:len(slices)-1]
		}
	}
	if err := WriteIndex(indexPath, slices); err != nil {
		return nil, err
	}
	return slices, nil
}

func removeStaleClips(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "clip_") || !strings.HasSuffix(name, ".ts") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func startClip(store *psi.Store, outDir string, number int, keyframe *demux.Keyframe) (*clipWriter, error) {
	snapshot := store.Snapshot()
	if !store.HasPAT() || len(snapshot.Programs) == 0 {
		return nil, ErrNoTables
	}
	name := fmt.Sprintf("clip_%03d.ts", number)
	path := filepath.Join(outDir, name)
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	info := SliceInfo{
		File:         name,
		PacketOffset: keyframe.PacketOffset,
		StartPTS:     keyframe.PTS,
		PMTVersions:  []PMTEntry{},
		hasPTS:       keyframe.HasPTS,
	}
	cw := &clipWriter{file: file, info: info, startPTS: keyframe.PTS, hasPTS: keyframe.HasPTS, packetSize: 188}
	if err := cw.writePSI(snapshot, number); err != nil {
		_ = file.Close()
		return nil, err
	}
	return cw, nil
}

func (c *clipWriter) write(packet *tspacket.Packet, store *psi.Store) error {
	if packet.TEI || packet.Duplicate || packet.PID == tspacket.NullPID || !packet.HasPayload {
		return nil
	}
	if store.IsPSIPID(packet.PID) {
		return nil
	}
	if _, _, mapped := store.LookupPID(packet.PID); !mapped {
		return nil
	}
	if _, err := c.file.Write(packet.Raw); err != nil {
		return err
	}
	c.info.PacketCount++
	c.info.ByteLength += 188
	return nil
}

func finishClip(c *clipWriter, endPTS int64, hasPTS bool) error {
	if err := c.file.Close(); err != nil {
		return err
	}
	if hasPTS && c.hasPTS && endPTS >= c.startPTS {
		c.info.DurationMS = (endPTS - c.startPTS) * 1000 / 90000
	}
	return nil
}

func mergeTail(dir string, previous *SliceInfo, tail SliceInfo) error {
	tailPath := filepath.Join(dir, tail.File)
	in, err := os.Open(tailPath)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := in.Seek(int64(tail.psiPackets)*188, io.SeekStart); err != nil {
		return err
	}
	prevPath := filepath.Join(dir, previous.File)
	out, err := os.OpenFile(prevPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	copied, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	previous.ByteLength += copied
	previous.PacketCount += tail.PacketCount - tail.psiPackets
	previous.DurationMS = tail.DurationMS + (tail.StartPTS-previous.StartPTS)*1000/90000
	return os.Remove(tailPath)
}

func WriteIndex(path string, slices []SliceInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".index-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(Index{Slices: slices}); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func ReadIndex(path string) (Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Index{}, err
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return Index{}, err
	}
	if index.Slices == nil {
		index.Slices = []SliceInfo{}
	}
	return index, nil
}
