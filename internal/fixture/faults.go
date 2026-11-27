package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
)

// injectBadChapterTimebase rewrites the ffmetadata chapter table embedded in
// the Matroska/MP4 fixture so one chapter declares a 0/0 time base. The
// scanner must reject that chapter and keep the rest of the file usable.
func injectBadChapterTimebase(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Chapter TIMEBASE strings appear verbatim in the container metadata.
	marker := []byte("TIMEBASE=1/1000")
	idx := indexBytes(b, marker)
	if idx < 0 {
		// MP4 stores chapters in a different form; inject a corrupt emsg on
		// the data track instead, which exercises the same validation path.
		return injectCorruptEmsg(path)
	}
	copy(b[idx:], []byte("TIMEBASE=0/0   "))
	return os.WriteFile(path, b, 0o644)
}

func injectCorruptEmsg(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// A data-track sample whose declared emsg length exceeds the payload.
	payload := make([]byte, 32)
	binary.BigEndian.PutUint32(payload[0:4], 4096)
	copy(payload[4:8], "emsg")
	payload[8] = 0 // version 0
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("fixture: append corrupt emsg: %w", err)
	}
	return nil
}

func indexBytes(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
