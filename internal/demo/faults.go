package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"mediacheck/internal/fixture"
	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
)

// CheckFaults executes selected invalid-material paths. Each returned line is
// stable terminal output. An error means an injected fault was not classified.
func CheckFaults(selected map[string]bool) ([]string, error) {
	var lines []string
	var failures []error

	if selected["truncated"] {
		if line, err := checkTruncatedNAL(); err != nil {
			failures = append(failures, err)
		} else {
			lines = append(lines, line)
		}
		if line, err := checkTruncatedOBU(); err != nil {
			failures = append(failures, err)
		} else {
			lines = append(lines, line)
		}
	}
	if selected["stale-index"] {
		if line, err := checkStaleIndex(); err != nil {
			failures = append(failures, err)
		} else {
			lines = append(lines, line)
		}
	}
	if selected["rtp-wrap"] {
		if line, err := checkRTPWrap(); err != nil {
			failures = append(failures, err)
		} else {
			lines = append(lines, line)
		}
	}
	if selected["slow-reader"] {
		r, err := RunSlowReader()
		if err != nil {
			failures = append(failures, err)
		} else {
			lines = append(lines, r.String())
		}
	}

	if len(failures) > 0 {
		return lines, failures[0]
	}
	return lines, nil
}

func checkTruncatedNAL() (string, error) {
	clock := media.DefaultClock()
	_, err := ingest.ParseElementary(ingest.ParseRequest{
		Codec: ingest.CodecH264, Data: fixture.TruncatedH264(), Clock: &clock, FrameDuration: 3000,
	})
	return expectedErrorLine("truncated-nal", media.KindTruncated, err)
}

func checkTruncatedOBU() (string, error) {
	clock := media.DefaultClock()
	_, err := ingest.ParseElementary(ingest.ParseRequest{
		Codec: ingest.CodecAV1, Data: fixture.TruncatedAV1(), Clock: &clock, FrameDuration: 3000,
	})
	return expectedErrorLine("truncated-obu", media.KindTruncated, err)
}

func checkStaleIndex() (string, error) {
	b := fixture.Normal()
	raw, err := json.Marshal(fixture.StaleIndex(b.Index))
	if err != nil {
		return "", err
	}
	_, err = ingest.LoadKeyIndex(bytes.NewReader(raw), time.Now())
	return expectedErrorLine("stale-index", media.KindStaleIndex, err)
}

func checkRTPWrap() (string, error) {
	idx, packets := fixture.WrapRTPIndex()
	err := idx.ValidateRTP(packets)
	return expectedErrorLine("rtp-wrap", media.KindWrap, err)
}

func expectedErrorLine(fault string, want media.Kind, err error) (string, error) {
	if err == nil {
		return "", media.E(media.StageDemo, media.KindValidation, -1,
			fmt.Errorf("fault %s was not rejected", fault))
	}
	me, ok := media.AsError(err)
	if !ok {
		return "", media.E(media.StageDemo, media.KindValidation, -1,
			fmt.Errorf("fault %s returned non-media error: %w", fault, err))
	}
	if me.Kind != want {
		return "", media.E(media.StageDemo, media.KindValidation, me.Offset,
			fmt.Errorf("fault %s kind=%s want=%s", fault, me.Kind, want))
	}
	return fmt.Sprintf(
		"fault=%s status=expected_invalid error_stage=%s error_kind=%s error_offset=%d retryable=%t expected_exit_code=2",
		fault, me.Stage, me.Kind, me.Offset, me.Retryable), nil
}
