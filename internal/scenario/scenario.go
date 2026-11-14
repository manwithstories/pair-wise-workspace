// Package scenario defines scripted capture scenarios: the deterministic RTP
// stream the mock server emits and the fault injections applied on the way.
package scenario

import (
	"encoding/json"
	"fmt"
	"os"
)

// Injection event types understood by the mock server and the capture client.
const (
	// EventDrop makes the server skip sending packets [Pos, Pos+Count).
	EventDrop = "drop"
	// EventReorder moves the packet at Pos forward by Shift positions.
	EventReorder = "reorder"
	// EventDuplicate re-sends the packet at Pos Count extra times.
	EventDuplicate = "duplicate"
	// EventLate delays sending of the packet at Pos by Shift send slots.
	EventLate = "late"
	// EventCut closes the control connection after packet Pos is sent; the
	// client is expected to reconnect and resume at sequence ResumeAt.
	EventCut = "cut"
	// EventAuthFail makes the server keep replying 401 even with credentials.
	EventAuthFail = "authfail"
)

// Event is one scripted fault injection.
type Event struct {
	Type     string `json:"type"`
	Pos      int    `json:"pos"`
	Count    int    `json:"count"`
	Shift    int    `json:"shift"`
	ResumeAt int    `json:"resume_at"`
}

// Expect carries the acceptance ranges checked by the verify subcommand.
type Expect struct {
	StateTransitions int     `json:"state_transitions"`
	RTPReceivedMin   int     `json:"rtp_received_min"`
	RTPReceivedMax   int     `json:"rtp_received_max"`
	Duplicates       int     `json:"rtp_duplicates"`
	Reordered        int     `json:"rtp_reordered"`
	SeqGapSegments   int     `json:"seq_gap_segments"`
	GapPackets       int     `json:"gap_packets"`
	MaxReorderDepth  int     `json:"max_reorder_depth"`
	FramesMin        int     `json:"frames_min"`
	FramesMax        int     `json:"frames_max"`
	BytesMin         int64   `json:"bytes_min"`
	BytesMax         int64   `json:"bytes_max"`
	FirstSeq         *uint16 `json:"first_seq,omitempty"`
	LastSeq          *uint16 `json:"last_seq,omitempty"`
}

// Scenario is a complete scripted capture description.
type Scenario struct {
	Name            string  `json:"name"`
	TotalPackets    int     `json:"total_packets"`
	PacketsPerFrame int     `json:"packets_per_frame"`
	PayloadSize     int     `json:"payload_size"`
	StartSeq        int     `json:"start_seq"`
	SSRC            uint32  `json:"ssrc"`
	ClockRate       uint32  `json:"clock_rate"`
	Username        string  `json:"username"`
	Password        string  `json:"password"`
	AuthEnabled     bool    `json:"auth_enabled"`
	ReadTimeoutMS   int     `json:"read_timeout_ms"`
	Events          []Event `json:"events"`
	Expect          Expect  `json:"expect"`
}

// Load reads and validates a scenario from a JSON file.
func Load(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario %q: %w", path, err)
	}
	var sc Scenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, fmt.Errorf("parse scenario %q: %w", path, err)
	}
	if err := sc.validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario %q: %w", path, err)
	}
	return &sc, nil
}

// Save writes the scenario as indented JSON.
func (sc *Scenario) Save(path string) error {
	raw, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func (sc *Scenario) validate() error {
	if sc.TotalPackets <= 0 {
		return fmt.Errorf("total_packets must be positive")
	}
	if sc.PacketsPerFrame <= 0 {
		return fmt.Errorf("packets_per_frame must be positive")
	}
	if sc.PayloadSize <= 0 {
		return fmt.Errorf("payload_size must be positive")
	}
	if sc.ClockRate == 0 {
		sc.ClockRate = 90000
	}
	if sc.ReadTimeoutMS <= 0 {
		sc.ReadTimeoutMS = 3000
	}
	known := map[string]bool{
		EventDrop: true, EventReorder: true, EventDuplicate: true,
		EventLate: true, EventCut: true, EventAuthFail: true,
	}
	for i, ev := range sc.Events {
		if !known[ev.Type] {
			return fmt.Errorf("events[%d]: unknown type %q", i, ev.Type)
		}
		if ev.Pos < 0 || ev.Pos >= sc.TotalPackets {
			return fmt.Errorf("events[%d]: pos %d out of range", i, ev.Pos)
		}
	}
	return nil
}

// EventOf returns the first event of the given type, or nil.
func (sc *Scenario) EventOf(typ string) *Event {
	for i := range sc.Events {
		if sc.Events[i].Type == typ {
			return &sc.Events[i]
		}
	}
	return nil
}
