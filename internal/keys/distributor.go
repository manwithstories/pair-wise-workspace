// Package keys also provides an in-process key distributor that hands out
// version leases with reuse, quota enforcement and lease reclamation.
package keys

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrQuotaExhausted is returned when the lease quota is exhausted.
var ErrQuotaExhausted = errors.New("quota_exhausted")

// Lease is a time-boxed grant to use a key version.
type Lease struct {
	VersionID string    `json:"version_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Reused    bool      `json:"reused"`
}

// Distributor issues leases for registry versions. It is safe for
// concurrent use.
type Distributor struct {
	mu     sync.Mutex
	reg    *Registry
	quota  int
	ttl    time.Duration
	leases map[string]*Lease
	now    func() time.Time
}

// NewDistributor creates a distributor over reg. quota <= 0 means unlimited.
func NewDistributor(reg *Registry, quota int, ttl time.Duration) *Distributor {
	return &Distributor{
		reg:    reg,
		quota:  quota,
		ttl:    ttl,
		leases: map[string]*Lease{},
		now:    time.Now,
	}
}

// Acquire returns a lease for versionID, reusing a live lease when one
// exists. Expired leases are reclaimed first and never count as rotations.
func (d *Distributor) Acquire(versionID string) (*Lease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.reclaimLocked(now)
	if l, ok := d.leases[versionID]; ok {
		cp := *l
		cp.Reused = true
		return &cp, nil
	}
	if d.quota > 0 && len(d.leases) >= d.quota {
		return nil, ErrQuotaExhausted
	}
	v := d.reg.Find(versionID)
	if v == nil {
		return nil, fmt.Errorf("unknown key version %q", versionID)
	}
	if v.State == StateRetired {
		return nil, fmt.Errorf("key version %q is retired", versionID)
	}
	l := &Lease{VersionID: versionID, IssuedAt: now, ExpiresAt: now.Add(d.ttl)}
	d.leases[versionID] = l
	cp := *l
	return &cp, nil
}

// Reclaim drops expired leases and returns how many were reclaimed.
func (d *Distributor) Reclaim() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reclaimLocked(d.now())
}

func (d *Distributor) reclaimLocked(now time.Time) int {
	n := 0
	for id, l := range d.leases {
		if !now.Before(l.ExpiresAt) {
			delete(d.leases, id)
			n++
		}
	}
	return n
}

// Outstanding returns the number of live leases.
func (d *Distributor) Outstanding() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reclaimLocked(d.now())
	return len(d.leases)
}
