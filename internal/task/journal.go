package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	Sequence uint64          `json:"seq"`
	TimeUTC  string          `json:"time_utc"`
	Kind     string          `json:"kind"`
	Segment  int             `json:"segment,omitempty"`
	From     State           `json:"from,omitempty"`
	To       State           `json:"to,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type Journal struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	seq    uint64
	states map[int]State
}

// OpenJournal validates complete records, discards a partial trailing line,
// and reconstructs the latest persisted state for every segment.
func OpenJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	validEnd, events, err := decodeJournal(data)
	if err != nil {
		return nil, err
	}
	if validEnd < len(data) {
		if err := os.Truncate(path, int64(validEnd)); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	j := &Journal{file: f, path: path, states: make(map[int]State)}
	for _, ev := range events {
		j.seq = ev.Sequence
		if ev.Kind == "transition" && ev.Segment > 0 {
			j.states[ev.Segment] = ev.To
		}
	}
	return j, nil
}

func decodeJournal(data []byte) (int, []Event, error) {
	events := make([]Event, 0)
	validEnd := 0
	for start := 0; start < len(data); {
		rel := bytes.IndexByte(data[start:], '\n')
		if rel < 0 {
			break
		}
		end := start + rel
		if end == start {
			start = end + 1
			validEnd = start
			continue
		}
		var ev Event
		if err := json.Unmarshal(data[start:end], &ev); err != nil {
			if end+1 == len(data) {
				break
			}
			return 0, nil, fmt.Errorf("journal corrupt at byte %d: %w", start, err)
		}
		events = append(events, ev)
		start = end + 1
		validEnd = start
	}
	return validEnd, events, nil
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

func (j *Journal) State(id int) State {
	j.mu.Lock()
	defer j.mu.Unlock()
	if s, ok := j.states[id]; ok {
		return s
	}
	return Pending
}

func (j *Journal) States() map[int]State {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[int]State, len(j.states))
	for id, state := range j.states {
		out[id] = state
	}
	return out
}

func (j *Journal) Transition(id int, to State, data any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	from := Pending
	if state, ok := j.states[id]; ok {
		from = state
	}
	if from == Done && to == Done {
		return nil
	}
	if err := CheckTransition(from, to); err != nil {
		return fmt.Errorf("segment %d: %w", id, err)
	}
	raw, err := marshalData(data)
	if err != nil {
		return err
	}
	ev := Event{Kind: "transition", Segment: id, From: from, To: to, Data: raw}
	if err := j.appendLocked(ev); err != nil {
		return err
	}
	j.states[id] = to
	return nil
}

// Record writes a non-state progress event, useful for probe and planning
// milestones while retaining a single append-only stream.
func (j *Journal) Record(kind string, id int, data any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	raw, err := marshalData(data)
	if err != nil {
		return err
	}
	return j.appendLocked(Event{Kind: kind, Segment: id, Data: raw})
}

func (j *Journal) appendLocked(ev Event) error {
	if j.file == nil {
		return fmt.Errorf("journal is closed")
	}
	j.seq++
	ev.Sequence = j.seq
	ev.TimeUTC = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := j.file.Write(line); err != nil {
		return err
	}
	return j.file.Sync()
}

func marshalData(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return data, nil
}
