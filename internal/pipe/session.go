package pipe

import (
	"fmt"
	"os"
	"strconv"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
	"archivecheck/internal/parse"
	"archivecheck/internal/repair"
)

// Task is one media segment scheduled for streaming parse. Group identifies
// the init group that owns it.
type Task struct {
	Group   int
	Segment string
	Probe   parse.Probe
}

// LoadedInput is a parsed archive: one init segment and its media probes per
// manifest group, plus the open files that stay alive for the whole run.
type LoadedInput struct {
	Manifest  *Manifest
	Inits     []*parse.Init
	Probes    map[string]parse.Probe
	Files     map[string]*os.File
	OwnerInit map[string]*parse.Init
	GroupOf   map[string]int
}

// Close releases every open segment file.
func (l *LoadedInput) Close() {
	for _, f := range l.Files {
		if f != nil {
			f.Close()
		}
	}
}

// LoadInputs opens and validates every manifest entry. Each init segment is
// fully parsed into its own track namespace. Media fragments are only probed
// for mfhd ordering here; full validation runs inside the streaming parse
// stage so faults reach the resumable checkpoint.
func LoadInputs(m *Manifest) (*LoadedInput, error) {
	in := &LoadedInput{
		Manifest: m, Files: map[string]*os.File{},
		Probes: map[string]parse.Probe{}, OwnerInit: map[string]*parse.Init{},
		GroupOf: map[string]int{},
	}
	for gi, g := range m.Groups {
		f, err := os.Open(g.Init.Path)
		if err != nil {
			in.Close()
			return nil, fault.Wrap(err, fault.KindNotRegular, g.Init.Name, "", 0, "cannot open init")
		}
		in.Files[g.Init.Name] = f
		init, err := parse.ParseInitReader(f, g.Init.Name)
		if err != nil {
			in.Close()
			return nil, err
		}
		in.Inits = append(in.Inits, init)
		for _, media := range g.Media {
			mf, err := os.Open(media.Path)
			if err != nil {
				in.Close()
				return nil, fault.Wrap(err, fault.KindNotRegular, media.Name, "", 0, "cannot open media")
			}
			in.Files[media.Name] = mf
			probe, err := parse.ProbeReader(mf, media.Name)
			if err != nil {
				in.Close()
				return nil, err
			}
			probe.Group = gi
			in.Probes[media.Name] = probe
			in.OwnerInit[media.Name] = init
			in.GroupOf[media.Name] = gi
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
			Group: l.GroupOf[e.Name], Segment: e.Name, HasMfhd: p.HasMfhd,
			Sequence: p.Sequence, MfhdOffset: p.MoofOffset, Index: i,
		})
	}
	return items
}

// Tasks returns streaming parse tasks in the given deterministic order.
func (l *LoadedInput) Tasks(ord *repair.Ordered) []Task {
	tasks := make([]Task, 0, len(ord.Items))
	for _, it := range ord.Items {
		tasks = append(tasks, Task{Group: it.Group, Segment: it.Segment, Probe: l.Probes[it.Segment]})
	}
	return tasks
}

// InitFor returns the init segment that owns the given media segment.
func (l *LoadedInput) InitFor(segment string) *parse.Init { return l.OwnerInit[segment] }

// TrackFor returns the track declared by a group's init that a fragment of
// that group carries. A restored program group is single-track by
// construction, so the first track is the fragment's track.
func (l *LoadedInput) TrackFor(group int) *parse.Track {
	if group < 0 || group >= len(l.Inits) {
		return nil
	}
	init := l.Inits[group]
	if t, ok := init.Tracks[init.Order[0]]; ok {
		return t
	}
	for _, t := range init.Tracks {
		return t
	}
	return nil
}

// CursorKey names a per-group, per-track resume cursor.
func CursorKey(group int, track uint32) string {
	return strconv.Itoa(group) + ":" + strconv.FormatUint(uint64(track), 10)
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

// ResumeStartFor validates both the manifest match and the on-disk index state.
func ResumeStartFor(m *Manifest, cp *index.Checkpoint, outPath string) (Start, error) {
	start, err := ResumeStart(m, cp)
	if err != nil {
		return start, err
	}
	if err := ValidateResumeState(outPath, cp); err != nil {
		return start, err
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
