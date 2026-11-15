// Package keys implements the key version registry with an
// active/grace/retired state machine and rotation accounting.
package keys

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// State is the lifecycle state of a key version.
type State string

const (
	StateActive  State = "active"
	StateGrace   State = "grace"
	StateRetired State = "retired"
)

// Version is a single key version.
type Version struct {
	ID         string    `json:"id"`
	Epoch      int       `json:"epoch"`
	KeyHex     string    `json:"key"`
	State      State     `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	GraceUntil time.Time `json:"grace_until,omitempty"`
}

// Key decodes the raw key bytes.
func (v *Version) Key() ([]byte, error) {
	return hex.DecodeString(v.KeyHex)
}

// Registry is the version table plus rotation bookkeeping.
type Registry struct {
	Versions    []*Version `json:"versions"`
	Rotations   int        `json:"rotations"`
	GracePeriod int64      `json:"grace_period_ns"`
}

// NewRegistry creates an empty registry with the given grace period.
func NewRegistry(grace time.Duration) *Registry {
	return &Registry{GracePeriod: int64(grace)}
}

// Active returns the active version or nil.
func (r *Registry) Active() *Version {
	for _, v := range r.Versions {
		if v.State == StateActive {
			return v
		}
	}
	return nil
}

// Find looks up a version by id.
func (r *Registry) Find(id string) *Version {
	for _, v := range r.Versions {
		if v.ID == id {
			return v
		}
	}
	return nil
}

// Rotate installs a new active version. The previous active version enters
// grace and stays decryptable until its grace deadline. Grace versions
// whose deadline has already passed are retired. Rotations counts
// transitions between versions, so the first version does not count.
func (r *Registry) Rotate(id, keyHex string, now time.Time) (*Version, error) {
	if r.Find(id) != nil {
		return nil, fmt.Errorf("key version %q already exists", id)
	}
	epoch := 1
	if r.Active() != nil {
		epoch = r.Active().Epoch + 1
		r.Rotations++
	}
	for _, v := range r.Versions {
		switch v.State {
		case StateActive:
			v.State = StateGrace
			v.GraceUntil = now.Add(time.Duration(r.GracePeriod))
		case StateGrace:
			if !v.GraceUntil.IsZero() && !now.Before(v.GraceUntil) {
				v.State = StateRetired
			}
		}
	}
	v := &Version{
		ID:        id,
		Epoch:     epoch,
		KeyHex:    keyHex,
		State:     StateActive,
		CreatedAt: now,
	}
	r.Versions = append(r.Versions, v)
	return v, nil
}

// Load reads a registry from disk.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode registry: %w", err)
	}
	seen := map[string]bool{}
	active := 0
	for _, v := range r.Versions {
		if seen[v.ID] {
			return nil, fmt.Errorf("registry inconsistent: duplicate version %q", v.ID)
		}
		seen[v.ID] = true
		if v.State == StateActive {
			active++
		}
	}
	if active > 1 {
		return nil, fmt.Errorf("registry inconsistent: %d active versions", active)
	}
	return &r, nil
}

// Save writes the registry as indented JSON.
func (r *Registry) Save(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
