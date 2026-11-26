package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/mp4"
)

// Set contains all generated sample paths.
type Set struct {
	Dir             string            `json:"dir"`
	NormalMP4       string            `json:"normal_mp4"`
	NormalMOV       string            `json:"normal_mov"`
	DamagedCombined string            `json:"damaged_combined"`
	FaultFiles      map[string]string `json:"fault_files"`
}

// GenerateSet creates valid and at least four classes of corrupted material.
func GenerateSet(dir string, cfg Config) (*Set, error) {
	if cfg.Duration == 0 {
		cfg.Duration = 60_000_000_000
	}
	if err := os.MkdirAll(filepath.Join(dir, "faults"), 0o755); err != nil {
		return nil, err
	}
	s := &Set{Dir: dir, FaultFiles: map[string]string{}}
	base := BaseProject(cfg.Seed, cfg.Duration)

	s.NormalMP4 = filepath.Join(dir, "normal.mp4")
	if err := mp4.WriteFile(s.NormalMP4, base); err != nil {
		return nil, err
	}
	mov := cloneProject(base)
	mov.SourceFormat = "mov"
	s.NormalMOV = filepath.Join(dir, "normal.mov")
	if err := mp4.WriteFile(s.NormalMOV, mov); err != nil {
		return nil, err
	}

	backward := WithBackwardTimestamp(base, 37)
	p := filepath.Join(dir, "faults", "backward_timestamps.mp4")
	if err := mp4.WriteFile(p, backward); err != nil {
		return nil, err
	}
	s.FaultFiles["backward_timestamps"] = p

	audioBad := WithAudioConflict(base)
	p = filepath.Join(dir, "faults", "audio_rate_channels.mp4")
	if err := mp4.WriteFile(p, audioBad); err != nil {
		return nil, err
	}
	s.FaultFiles["audio_rate_channels"] = p

	colorBad := WithColorConflict(base)
	p = filepath.Join(dir, "faults", "color_metadata.mp4")
	if err := mp4.WriteFile(p, colorBad); err != nil {
		return nil, err
	}
	s.FaultFiles["color_metadata"] = p

	badOffsetsPath := filepath.Join(dir, "faults", "bad_sample_offsets.mp4")
	if err := mp4.WriteFile(badOffsetsPath, base); err != nil {
		return nil, err
	}
	if _, err := CorruptSTCOffset(badOffsetsPath, 0, 42); err != nil {
		return nil, err
	}
	s.FaultFiles["bad_sample_offsets"] = badOffsetsPath

	truncatedPath := filepath.Join(dir, "faults", "truncated_mdat.mp4")
	if err := mp4.WriteFile(truncatedPath, base); err != nil {
		return nil, err
	}
	if st, err := os.Stat(truncatedPath); err == nil {
		tail := int64(4096)
		if adaptive := st.Size() / 5; adaptive > 0 && adaptive < tail {
			tail = adaptive
		}
		if err := TruncateMDAT(truncatedPath, tail); err != nil {
			return nil, err
		}
	}
	s.FaultFiles["truncated_mdat"] = truncatedPath

	missingIndexPath := filepath.Join(dir, "faults", "missing_stss.mp4")
	if err := mp4.WriteFile(missingIndexPath, base); err != nil {
		return nil, err
	}
	if _, err := CorruptSTSSType(missingIndexPath); err != nil {
		return nil, err
	}
	s.FaultFiles["missing_index"] = missingIndexPath

	combined := WithColorConflict(WithAudioConflict(WithBackwardTimestamp(base, 37)))
	s.DamagedCombined = filepath.Join(dir, "damaged.mp4")
	if err := mp4.WriteFile(s.DamagedCombined, combined); err != nil {
		return nil, err
	}
	if _, err := CorruptSTCOffset(s.DamagedCombined, 0, 42); err != nil {
		return nil, err
	}
	return s, nil
}

// CorruptSTCOffset writes an out-of-bounds value into a deterministic stco entry.
// trackTableZero selects the nth stco box; entry selects the chunk offset.
func CorruptSTCOffset(path string, trackTableZero, entry int) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	boxes, err := mp4.Scan(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("scan before offset corruption: %w", err)
	}
	var stcos []*mp4.Box
	for _, b := range boxes {
		if b.Type == "moov" {
			stcos = append(stcos, b.All("stco")...)
		}
	}
	if trackTableZero < 0 || trackTableZero >= len(stcos) {
		return 0, fmt.Errorf("stco table %d not found", trackTableZero)
	}
	stco := stcos[trackTableZero]
	countLocation := stco.Offset + stco.HeaderSize + 4
	entryCount := int(binary.BigEndian.Uint32(data[countLocation : countLocation+4]))
	if entry < 0 || entry >= entryCount {
		entry = entryCount / 2
	}
	location := stco.Offset + stco.HeaderSize + 8 + int64(entry)*4
	if location+4 > int64(len(data)) {
		return 0, fmt.Errorf("stco entry %d is outside file", entry)
	}
	binary.BigEndian.PutUint32(data[location:location+4], 0x7fff_ff00)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 0, err
	}
	return location, nil
}

// TruncateMDAT removes tailBytes from the file while preserving ftyp/moov and
// the mdat header plus a small payload prefix.
func TruncateMDAT(path string, tailBytes int64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	stSize := int64(len(data))
	if tailBytes <= 0 || tailBytes >= stSize {
		return fmt.Errorf("invalid truncation tail=%d size=%d", tailBytes, stSize)
	}
	boxes, err := mp4.Scan(bytes.NewReader(data), stSize)
	if err != nil {
		return fmt.Errorf("scan before truncation: %w", err)
	}
	var mdat *mp4.Box
	for _, b := range boxes {
		if b.Type == "mdat" {
			mdat = b
		}
	}
	if mdat == nil {
		return fmt.Errorf("mdat box not found")
	}
	newSize := stSize - tailBytes
	minimum := mdat.Offset + mdat.HeaderSize + 64
	if newSize < minimum {
		return fmt.Errorf("refusing truncation below recoverable mdat prefix: new=%d min=%d", newSize, minimum)
	}
	return os.Truncate(path, newSize)
}

// CorruptSTSSType renames the sync sample box so the index must be rebuilt.
func CorruptSTSSType(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	boxes, err := mp4.Scan(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("scan before stss corruption: %w", err)
	}
	var stss []*mp4.Box
	for _, b := range boxes {
		if b.Type == "moov" {
			stss = append(stss, b.All("stss")...)
		}
	}
	if len(stss) == 0 {
		return 0, fmt.Errorf("stss not found")
	}
	location := stss[0].Offset + 4 // type field
	copy(data[location:location+4], "free")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 0, err
	}
	return location, nil
}
