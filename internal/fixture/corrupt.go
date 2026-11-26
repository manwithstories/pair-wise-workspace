package fixture

import (
	"encoding/json"
	"os"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/timeline"
)

func writeCorruptSets(set *Set, records []timeline.Record) error {
	total := len(records)
	crcIdx := total/3 - 1
	reverseIdx := total/2 - 1
	transitionIdx := (2 * total / 3) - 1

	crcRecords := append([]timeline.Record(nil), records...)
	crcRecords[crcIdx].CRC32 ^= 0xdeadbeef
	if err := WriteNDJSON(set.CRCBadPath, crcRecords); err != nil {
		return err
	}

	reverseRecords := append([]timeline.Record(nil), records...)
	prev := reverseRecords[reverseIdx-StreamCount]
	reverseRecords[reverseIdx].PTS90k = prev.PTS90k - RecordDurationTicks
	reverseRecords[reverseIdx].ComputeCRC()
	if err := WriteNDJSON(set.ReversePath, reverseRecords); err != nil {
		return err
	}

	transitionRecords := append([]timeline.Record(nil), records...)
	transitionRecords[transitionIdx].Transition = &timeline.Transition{
		ID: "fixture-missing-endpoint", Kind: "fade", Endpoint: "",
	}
	transitionRecords[transitionIdx].ComputeCRC()
	if err := WriteNDJSON(set.TransitionPath, transitionRecords); err != nil {
		return err
	}

	if err := writeTruncated(set.TruncatedPath, records); err != nil {
		return err
	}

	boundaryRecords := append([]timeline.Record(nil), records...)
	last := len(boundaryRecords) - 1
	boundaryRecords[last].Dur90k++ // exactly one late tick; alignment allows it.
	boundaryRecords[last].ComputeCRC()
	if err := WriteNDJSON(set.BoundaryPath, boundaryRecords); err != nil {
		return err
	}

	set.Expectations = []Expectation{
		{Case: "crc32", Path: set.CRCBadPath, RecordNo: crcIdx + 1, WantCode: media.CodeOf(media.ErrCRC32), WantStages: 3},
		{Case: "reverse", Path: set.ReversePath, RecordNo: reverseIdx + 1, WantCode: media.CodeOf(media.ErrTimestampOrder)},
		{Case: "transition", Path: set.TransitionPath, RecordNo: transitionIdx + 1, WantCode: media.CodeOf(media.ErrTransitionEndpoint), WantStages: 3},
		{Case: "truncated", Path: set.TruncatedPath, RecordNo: total, WantCode: media.CodeOf(media.ErrTruncatedRecord)},
	}
	return nil
}

func writeTruncated(path string, records []timeline.Record) error {
	if len(records) < 2 {
		return os.ErrInvalid
	}
	if err := WriteNDJSON(path, records[:len(records)-1]); err != nil {
		return err
	}
	last, err := json.Marshal(records[len(records)-1])
	if err != nil {
		return err
	}
	// Cut the final JSON object in half and deliberately omit the newline.
	cut := len(last) / 2
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(last[:cut]); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
