// Package audio performs streaming silence detection on little-endian
// signed 16-bit mono PCM and emits intervals on the unified timeline.
package audio

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"mediad/internal/model"
)

// Interval is a silent span expressed in unified-timeline milliseconds.
type Interval struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// Config tunes the detector. Zero fields fall back to documented defaults.
type Config struct {
	SampleRate   int64 // Hz, must be > 0
	Threshold    int16 // |sample| <= Threshold counts as silent
	MinSilenceMS int64 // runs shorter than this are not emitted
}

// Detector holds validated detection parameters.
type Detector struct {
	rate      int64
	threshold int16
	minMS     int64
}

// NewDetector validates the configuration.
func NewDetector(cfg Config) (*Detector, error) {
	if cfg.SampleRate <= 0 {
		return nil, fmt.Errorf("silence: sample rate must be positive, got %d", cfg.SampleRate)
	}
	if cfg.MinSilenceMS < 0 {
		return nil, fmt.Errorf("silence: min silence must not be negative, got %d", cfg.MinSilenceMS)
	}
	return &Detector{rate: cfg.SampleRate, threshold: cfg.Threshold, minMS: cfg.MinSilenceMS}, nil
}

// Detect consumes the full PCM stream from r. offset is the timeline position
// of sample zero (typically the owning segment's Start). Memory use is a
// small read buffer plus the emitted interval list, independent of duration.
func (d *Detector) Detect(r io.Reader, offset model.Rat) ([]Interval, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		out      []Interval
		silent   bool
		silentAt int64 // first silent sample index of the current run
		idx      int64
	)
	flush := func(endSample int64) error {
		if !silent {
			return nil
		}
		iv, err := d.interval(offset, silentAt, endSample)
		silent = false
		if err != nil {
			return err
		}
		if iv.EndMS-iv.StartMS < d.minMS {
			return nil // too short: never becomes its own segment
		}
		out = append(out, iv)
		return nil
	}
	for {
		v, err := readSample16LE(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			if err == io.ErrUnexpectedEOF {
				break // dangling high byte at stream end carries no sample
			}
			return nil, err
		}
		if abs16(v) <= d.threshold {
			if !silent {
				silent, silentAt = true, idx
			}
		} else if err := flush(idx); err != nil {
			return nil, err
		}
		idx++
	}
	if err := flush(idx); err != nil {
		return nil, err
	}
	return out, nil
}

// interval maps a half-open [from,to) sample range onto timeline
// milliseconds through checked rational arithmetic.
func (d *Detector) interval(offset model.Rat, from, to int64) (Interval, error) {
	fromRat, err := model.NewRat(from, d.rate)
	if err != nil {
		return Interval{}, err
	}
	toRat, err := model.NewRat(to, d.rate)
	if err != nil {
		return Interval{}, err
	}
	start, err := offset.Add(fromRat)
	if err != nil {
		return Interval{}, err
	}
	end, err := offset.Add(toRat)
	if err != nil {
		return Interval{}, err
	}
	sms, err := start.Millis()
	if err != nil {
		return Interval{}, err
	}
	ems, err := end.Millis()
	if err != nil {
		return Interval{}, err
	}
	return Interval{StartMS: sms, EndMS: ems}, nil
}

func abs16(v int16) int16 {
	if v < 0 {
		return -v
	}
	return v
}

// readSample16LE reads one little-endian signed 16-bit sample.
func readSample16LE(r io.Reader) (int16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int16(binary.LittleEndian.Uint16(b[:])), nil
}
