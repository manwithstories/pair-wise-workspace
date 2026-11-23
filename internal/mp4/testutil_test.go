package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

// corruptFirstSync rewrites the first sample number of an stss box so tests can
// exercise the keyframe-table validator against a structurally valid file.
// It returns (false, nil) when no stss box is present.
func corruptFirstSync(path string, sampleNumber uint16) (bool, error) {
	data, err := readAll(path)
	if err != nil {
		return false, err
	}
	idx := bytes.Index(data, []byte("stss"))
	if idx < 4 || idx+4 >= len(data) {
		return false, nil
	}
	// Layout after the type: version(1)+flags(3)+entry_count(4)+entry(4)
	entry := idx + 4 + 8
	if entry+4 > len(data) {
		return false, nil
	}
	binary.BigEndian.PutUint32(data[entry:entry+4], uint32(sampleNumber))
	return true, writeAll(path, data)
}

func readAll(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func writeAll(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
