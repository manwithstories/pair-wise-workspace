package store

import "encoding/json"

// StatusCommitted marks a task whose result has been atomically persisted.
const StatusCommitted = "committed"

// Checkpoint is the per-task durable record. Result is opaque to the store so
// pipeline/report schemas can evolve without coupling persistence code.
type Checkpoint struct {
	SchemaVersion int             `json:"schema_version"`
	Task          Task            `json:"task"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
}

// SaveAtomic canonicalizes and atomically writes this checkpoint to path.
func (c Checkpoint) SaveAtomic(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(path, data, 0o644)
}
