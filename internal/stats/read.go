package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type logRecord struct {
	Sequence   uint16 `json:"seq"`
	Timestamp  uint32 `json:"timestamp"`
	ReceivedNS int64  `json:"received_ns"`
}

func ReadJSONL(path string, clockRate uint32) (Summary, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return Summary{}, 0, err
	}
	defer file.Close()
	tracker := NewTracker(clockRate)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var records uint64
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record logRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return Summary{}, records, fmt.Errorf("packet log line %d: %w", records+1, err)
		}
		tracker.Update(record.Sequence, record.Timestamp, time.Unix(0, record.ReceivedNS))
		records++
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, records, err
	}
	return tracker.Summary(), records, nil
}
