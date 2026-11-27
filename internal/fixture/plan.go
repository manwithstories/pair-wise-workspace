package fixture

// Profile selects which fault set a generated dataset carries.
type Profile string

const (
	// ProfileClean injects no faults.
	ProfileClean Profile = "clean"
	// ProfileBoundary injects the full acceptance boundary-condition set.
	ProfileBoundary Profile = "boundary"
)

// SegmentFault describes one injected condition keyed by camera and sequence.
type SegmentFault struct {
	Camera   string
	Sequence int
	// Kind is one of: "lost", "truncated", "out_of_order", "pts_reset",
	// "color_conflict", "duplicate".
	Kind string
	// Recoverable reports whether the backup representation can fill the slot.
	Recoverable bool
}

// Plan is the deterministic fault layout for one generated dataset.
type Plan struct {
	Profile        Profile
	Cameras        []string
	SegmentCount   int // segments per representation
	SegmentSec     float64
	Faults         []SegmentFault
	// LostSlotSeqs lists every primary slot whose segment is absent.
	LostSlotSeqs []int
	// UnrecoverableSeqs lists slots absent from primary AND backup.
	UnrecoverableSeqs []int
}

// CameraNames maps a camera count to stable identifiers.
func CameraNames(n int) []string {
	all := []string{"stage", "side", "audience"}
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// BuildPlan computes a deterministic fault layout. The boundary profile always
// produces ten lost slots of which nine are recoverable from a backup
// representation, plus one each of truncation, out-of-order sequence, PTS
// reset and colour conflict.
func BuildPlan(profile Profile, cameras []string, segmentCount int) *Plan {
	p := &Plan{
		Profile:      profile,
		Cameras:      cameras,
		SegmentCount: segmentCount,
		SegmentSec:   2.0,
	}
	if profile != ProfileBoundary || len(cameras) == 0 {
		return p
	}

	// Ten lost slots spread round-robin across cameras. The last one is made
	// unrecoverable by also removing its backup; the other nine recover.
	const lostTotal = 10
	for i := 0; i < lostTotal; i++ {
		cam := cameras[i%len(cameras)]
		// Pick distinct, in-range sequences deterministic per camera.
		seq := (i*3 + 1) % segmentCount
		recoverable := i != lostTotal-1
		p.Faults = append(p.Faults, SegmentFault{
			Camera: cam, Sequence: seq, Kind: "lost", Recoverable: recoverable,
		})
		p.LostSlotSeqs = append(p.LostSlotSeqs, seq)
		if !recoverable {
			p.UnrecoverableSeqs = append(p.UnrecoverableSeqs, seq)
		}
	}

	// Truncated packet on an early, non-lost slot of the first camera.
	p.Faults = append(p.Faults, SegmentFault{
		Camera: cameras[0], Sequence: 2, Kind: "truncated", Recoverable: true,
	})
	// Out-of-order sequence on the last camera's primary HLS playlist.
	p.Faults = append(p.Faults, SegmentFault{
		Camera: cameras[len(cameras)-1], Sequence: 5, Kind: "out_of_order", Recoverable: true,
	})
	// PTS reset marker on a mid-stream slot of the first camera.
	p.Faults = append(p.Faults, SegmentFault{
		Camera: cameras[0], Sequence: 8, Kind: "pts_reset", Recoverable: true,
	})
	// Colour conflict: backup of the second camera (or first) is tagged BT.2020.
	colorCam := cameras[0]
	if len(cameras) > 1 {
		colorCam = cameras[1]
	}
	p.Faults = append(p.Faults, SegmentFault{
		Camera: colorCam, Sequence: 0, Kind: "color_conflict", Recoverable: true,
	})
	return p
}

// FaultsFor filters the plan to one camera and fault kind.
func (p *Plan) FaultsFor(camera, kind string) []SegmentFault {
	var out []SegmentFault
	for _, f := range p.Faults {
		if f.Camera == camera && f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// HasFault reports whether a camera/sequence carries a given fault kind.
func (p *Plan) HasFault(camera string, seq int, kind string) bool {
	for _, f := range p.Faults {
		if f.Camera == camera && f.Sequence == seq && f.Kind == kind {
			return true
		}
	}
	return false
}

// IsLost reports whether the primary slot is absent.
func (p *Plan) IsLost(camera string, seq int) bool {
	return p.HasFault(camera, seq, "lost")
}

// IsUnrecoverable reports whether neither representation can fill the slot.
func (p *Plan) IsUnrecoverable(camera string, seq int) bool {
	for _, s := range p.UnrecoverableSeqs {
		if s == seq {
			// Only the camera that owns the unrecoverable slot.
			for _, f := range p.Faults {
				if f.Kind == "lost" && !f.Recoverable && f.Camera == camera && f.Sequence == seq {
					return true
				}
			}
		}
	}
	return false
}
