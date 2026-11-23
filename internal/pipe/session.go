package pipe

import (
	"fmt"
	"os"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
	"archivecheck/internal/parse"
	"archivecheck/internal/repair"
)

// Task is one media segment scheduled for streaming parse.
type Task struct {
	Segment string
	Probe   parse.Probe
}

// LoadedInput is one parsed archive input set: per group, an init segment and
// the media fragment probes that determine deterministic order.
type LoadedInput struct {
	Manifest  *Manifest
	Inits     []*parse.Init
	Probes    map[string]parse.Probe
	Files     map[string]*os.File
	OwnerInit map[string]*parse.Init
}

// Close releases every open segment file.
func (l *LoadedInput) Close() {
	for _, f := range l.Files {
		if f != nil {
			f.Close()
		}
	}
}

// ErrMultipleInitGroups is returned by LoadInputs when a manifest mixes init
// groups; a single restored program must map to exactly one track layout.
var ErrMultipleInitGroups = fault.New(fault.KindPath, "", "moov", 0,
	"manifest mixes multiple init segments; restore one program per run")

// LoadInputs opens and validates the manifest entries. Init segments are
// fully parsed; media fragments are probed only far enough to read mfhd
// ordering. Full fragment validation happens in the streaming parse stage so
// faults flow through the pipeline and into the resumable checkpoint.
func LoadInputs(m *Manifest) (*LoadedInput, error) {
	if len(m.Groups) != 1 {
		return nil, ErrMultipleInitGroups
	}
	in := &LoadedInput{
		Manifest: m, Files: map[string]*os.File{},
		Probes: map[string]parse.Probe{}, OwnerInit: map[string]*parse.Init{},
	}
	closeAll := func() { in.Close() }
	for _, g := range m.Groups {
		f, err := os.Open(g.Init.Path)
		if err != nil {
			closeAll()
			return nil, fault.Wrap(err, fault.KindNotRegular, g.Init.Name, "", 0, "cannot open init")
		}
		in.Files[g.Init.Name] = f
		init, err := parse.ParseInitReader(f, g.Init.Name)
		if err != nil {
			closeAll()
			return nil, err
		}
		in.Inits = append(in.Inits, init)
		for _, media := range g.Media {
			mf, err := os.Open(media.Path)
			if err != nil {
				closeAll()
				return nil, fault.Wrap(err, fault.KindNotRegular, media.Name, "", 0, "cannot open media")
			}
			in.Files[media.Name] = mf
			minfo, err := mf.Stat()
			if err != nil {
				closeAll()
				return nil, fault.Wrap(err, fault.KindShortRead, media.Name, "", 0, "cannot stat media")
			}
			probe, err := parse.ProbeSegment(mf, minfo.Size(), media.Name)
			if err != nil {
				closeAll()
				return nil, err
			}
			in.Probes[media.Name] = probe
			in.OwnerInit[media.Name] = init
		}
	}
	return in, nil
}

// OrderItems converts probes into deterministic ordering items in manifest
// order, preserving duplicate entries for the repair layer to resolve.
func (l *LoadedInput) OrderItems() []repair.Item {
	var items []repair.Item
	for i, e := range l.Manifest.Entries {
		if e.Kind != KindMedia {
			continue
		}
		p := l.Probes[e.Name]
		items = append(items, repair.Item{
			Segment: e.Name, HasMfhd: p.HasMfhd, Sequence: p.Sequence,
			MfhdOffset: p.MoofOffset, Index: i,
		})
	}
	return items
}

// Tasks returns streaming parse tasks in the given deterministic order.
func (l *LoadedInput) Tasks(ord *repair.Ordered) []Task {
	tasks := make([]Task, 0, len(ord.Items))
	for _, it := range ord.Items {
		tasks = append(tasks, Task{Segment: it.Segment, Probe: l.Probes[it.Segment]})
	}
	return tasks
}

// InitFor returns the init segment that owns the given media segment. Since a
// restored program has exactly one init group, this is always Inits[0].
func (l *LoadedInput) InitFor(segment string) *parse.Init {
	if init, ok := l.OwnerInit[segment]; ok {
		return init
	}
	if len(l.Inits) > 0 {
		return l.Inits[0]
	}
	return nil
}

// StartFor builds a fresh resumable start state.
func (l *LoadedInput) StartFor(ord *repair.Ordered) Start {
	return Start{InputDigest: l.Manifest.Digest, MissingMfhd: ord.Missing}
}

// ResumeStart validates that an on-disk checkpoint belongs to this exact input
// manifest and returns the resumable start state. A mismatch is reported as a
// checkpoint fault so the CLI can exit with code 3 instead of silently
// restarting and overwriting good output.
func ResumeStart(m *Manifest, cp *index.Checkpoint) (Start, error) {
	start := Start{Resuming: true, InputDigest: m.Digest}
	if cp.InputDigest == "" || cp.InputDigest != m.Digest {
		return start, fault.New(fault.KindCheckpoint, "", "", 0,
			fmt.Sprintf("checkpoint input digest %q does not match manifest %q", cp.InputDigest, m.Digest))
	}
	media := countMedia(m)
	if cp.FragmentsTotal != media {
		return start, fault.New(fault.KindCheckpoint, "", "", 0,
			fmt.Sprintf("checkpoint expects %d fragments, manifest has %d", cp.FragmentsTotal, media))
	}
	if cp.Committed < 0 || cp.Committed > cp.FragmentsTotal {
		return start, fault.New(fault.KindCheckpoint, "", "", 0,
			fmt.Sprintf("checkpoint committed %d outside 0..%d", cp.Committed, cp.FragmentsTotal))
	}
	start.Committed = cp.Committed
	start.CommittedDigest = cp.CommittedDigest
	start.OutputBytes = cp.OutputBytes
	start.NextDecodeTime = cp.NextDecodeTime
	start.RepairedTfdt = cp.RepairedTfdt
	start.MissingMfhd = cp.MissingMfhd
	if cp.Index != nil {
		start.Entries = cp.Index.Entries
	}
	return start, nil
}

func countMedia(m *Manifest) int {
	n := 0
	for _, g := range m.Groups {
		n += len(g.Media)
	}
	return n
}
