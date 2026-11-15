package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	RunStarted     = "run_started"
	SegmentStarted = "segment_started"
	SegmentDone    = "segment_done"
	SegmentFailed  = "segment_failed"
	Checkpoint     = "checkpoint"
)

var ErrPlanMismatch = errors.New("journal and plan differ")

// SegmentResult is both a journal payload and a report row.
type SegmentResult struct {
	Index      int     `json:"index"`
	Frames     int     `json:"frames"`
	Bytes      int64   `json:"bytes"`
	TargetKbps float64 `json:"target_kbps"`
	ActualKbps float64 `json:"actual_kbps"`
	DevPct     float64 `json:"dev_pct"`
	PSNRDB     float64 `json:"psnr_db"`
	PSNRMinDB  float64 `json:"psnr_min_db,omitempty"`
	PSNRMaxDB  float64 `json:"psnr_max_db,omitempty"`
	Q          float64 `json:"q"`
	Attempts   int     `json:"attempts"`
	Capped     bool    `json:"capped"`
	Status     Status  `json:"status"`
}

type Record struct {
	Type    string          `json:"type"`
	Time    time.Time       `json:"time"`
	PlanID  string          `json:"plan_id,omitempty"`
	Segment int             `json:"segment"`
	State   Status          `json:"state,omitempty"`
	Result  *SegmentResult  `json:"result,omitempty"`
	Results []SegmentResult `json:"results,omitempty"`
	Message string          `json:"message,omitempty"`
}

// Snapshot is the replayable state derived from a journal.
type Snapshot struct {
	PlanID     string
	PlanIDs    []string
	Completed  map[int]SegmentResult
	Failed     map[int]bool
	Checkpoint []int
	Warnings   []string
}

type ExpectedSegment struct {
	Frames     int
	TargetKbps float64
}

func (s *Snapshot) validate(planID string, segmentCount int) error {
	for _, id := range s.PlanIDs {
		if id != planID {
			return fmt.Errorf("%w: journal plan %s, requested plan %s", ErrPlanMismatch, id, planID)
		}
	}
	for index := range s.Completed {
		if index < 0 || index >= segmentCount {
			return fmt.Errorf("%w: completed segment %d outside plan", ErrPlanMismatch, index)
		}
	}
	for index := range s.Failed {
		if index < 0 || index >= segmentCount {
			return fmt.Errorf("%w: failed segment %d outside plan", ErrPlanMismatch, index)
		}
	}
	return nil
}

// ValidateSegments catches semantically different plan rows even if the ID was copied.
func (s *Snapshot) ValidateSegments(planID string, expected []ExpectedSegment) error {
	if err := s.validate(planID, len(expected)); err != nil {
		return err
	}
	for index, got := range s.Completed {
		want := expected[index]
		if got.Frames != want.Frames || math.Abs(got.TargetKbps-want.TargetKbps) > 1e-6 {
			return fmt.Errorf("%w: segment %d result does not match plan", ErrPlanMismatch, index)
		}
	}
	return nil
}

type Journal struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func Open(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Journal{file: f, path: path}, nil
}

func (j *Journal) Path() string { return j.path }

func (j *Journal) Append(rec Record) error {
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.file.Write(line); err != nil {
		return err
	}
	return j.file.Sync()
}

func (j *Journal) WriteCheckpoint(planID string, results []SegmentResult) error {
	sort.Slice(results, func(i, k int) bool { return results[i].Index < results[k].Index })
	indexes := make([]int, len(results))
	for i, r := range results {
		indexes[i] = r.Index
	}
	return j.Append(Record{Type: Checkpoint, PlanID: planID, Results: append([]SegmentResult(nil), results...),
		Message: fmt.Sprint(indexes)})
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

// Read replays a journal while skipping and reporting malformed lines.
func Read(path string) (*Snapshot, error) {
	s := &Snapshot{Completed: make(map[int]SegmentResult), Failed: make(map[int]bool)}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seenIDs := make(map[string]bool)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			s.Warnings = append(s.Warnings, fmt.Sprintf("journal line %d skipped: %v", lineNo, err))
			continue
		}
		if rec.PlanID != "" && !seenIDs[rec.PlanID] {
			seenIDs[rec.PlanID] = true
			s.PlanIDs = append(s.PlanIDs, rec.PlanID)
			if s.PlanID == "" {
				s.PlanID = rec.PlanID
			}
		}
		switch rec.Type {
		case SegmentDone:
			if rec.Result == nil || rec.Result.Index != rec.Segment {
				s.Warnings = append(s.Warnings, fmt.Sprintf("journal line %d skipped: inconsistent segment result", lineNo))
				continue
			}
			s.Completed[rec.Result.Index] = *rec.Result
			delete(s.Failed, rec.Result.Index)
		case SegmentFailed:
			if _, ok := s.Completed[rec.Segment]; !ok {
				s.Failed[rec.Segment] = true
			}
		case Checkpoint:
			if len(rec.Results) > 0 {
				for _, r := range rec.Results {
					if _, ok := s.Completed[r.Index]; !ok {
						s.Completed[r.Index] = r
					}
				}
				s.Checkpoint = s.Checkpoint[:0]
				for _, r := range rec.Results {
					s.Checkpoint = append(s.Checkpoint, r.Index)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) ValidatePlan(planID string, segmentCount int) error {
	return s.validate(planID, segmentCount)
}

func (s *Snapshot) CompletedList() []SegmentResult {
	out := make([]SegmentResult, 0, len(s.Completed))
	for _, r := range s.Completed {
		out = append(out, r)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Index < out[k].Index })
	return out
}
