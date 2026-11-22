// Package manifest turns aligned boundaries, edit points and transitions into
// the per-fragment manifest that delivery and on-call engineers read. Every
// entry is deterministic: the same timeline, boundaries and options always
// produce the same ordering, IDs and checksums.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"medialine/internal/align"
	"medialine/internal/fault"
	"medialine/internal/index"
	"medialine/internal/model"
)

// ManifestVersion is bumped when the on-disk schema changes incompatibly.
const ManifestVersion = 1

// Options controls segment construction.
type Options struct {
	// SegmentUS is the nominal segment length; boundaries come from align.
	SegmentUS int64
	// SnapBack snaps cut points that fall mid-sample back to a keyframe.
	SnapBack bool
	// OutDir is the directory that segment files are written beneath.
	OutDir string
	// Ext is the segment file extension, without a dot.
	Ext string
}

// Entry is one manifest row: a fragment for one representation.
type Entry struct {
	ID               string            `json:"id"`
	Representation   string            `json:"representation"`
	Bitrate          int64             `json:"bitrate"`
	TrackID          int               `json:"track_id"`
	Kind             model.SegmentKind `json:"kind"`
	StartUS          int64             `json:"start_us"`
	EndUS            int64             `json:"end_us"`
	RequestedStartUS int64             `json:"requested_start_us"`
	SnapOffsetUS     int64             `json:"snap_offset_us"`
	Switchable       bool              `json:"switchable"`
	SourceTrackIDs   []int             `json:"source_track_ids,omitempty"`
	TransitionKind   string            `json:"transition_kind,omitempty"`
	SampleNumbers    []int             `json:"sample_numbers"`
	File             string            `json:"file"`
	ContentHash      string            `json:"content_hash"`
	Status           string            `json:"status"`
	// Failure explains why the fragment is not deliverable. Empty means OK.
	Failure string `json:"failure,omitempty"`
}

// Representation describes a ladder rung in the manifest.
type Representation struct {
	ID        string `json:"id"`
	Bitrate   int64  `json:"bitrate"`
	TrackID   int    `json:"track_id"`
	Codec     string `json:"codec"`
	Timescale uint32 `json:"timescale"`
}

// TransitionRecord describes one composite region.
type TransitionRecord struct {
	StartUS int64  `json:"start_us"`
	EndUS   int64  `json:"end_us"`
	Kind    string `json:"kind"`
	Sources []int  `json:"sources"`
	// Representative is the representation used for the composite payload.
	Representative string `json:"representative"`
}

// Manifest is the JSON document written to disk.
type Manifest struct {
	Version         int                `json:"version"`
	SegmentUS       int64              `json:"segment_us"`
	SpanUS          int64              `json:"span_us"`
	Representations []Representation   `json:"representations"`
	Transitions     []TransitionRecord `json:"transitions,omitempty"`
	Entries         []Entry            `json:"entries"`
	// Issues carries recoverable faults that did not prevent output.
	Issues []string `json:"issues,omitempty"`
}

// Builder accumulates entries before they are serialised.
type Builder struct {
	opts    Options
	reps    []Representation
	entries []Entry
	issues  []string
	seen    map[string]bool
}

// NewBuilder validates options and creates a builder.
func NewBuilder(opts Options) (*Builder, error) {
	if opts.SegmentUS <= 0 {
		return nil, fault.New(fault.StageAlign, fault.KindBadConfig,
			"segment length %dus must be positive", opts.SegmentUS)
	}
	if opts.OutDir == "" {
		return nil, fault.New(fault.StageWrite, fault.KindBadConfig, "output directory is required")
	}
	if opts.Ext == "" {
		opts.Ext = "m4s"
	}
	return &Builder{opts: opts, seen: make(map[string]bool)}, nil
}

// AddRepresentation registers a ladder rung. Duplicate IDs are rejected.
func (b *Builder) AddRepresentation(rep Representation) error {
	for _, r := range b.reps {
		if r.ID == rep.ID {
			return fault.New(fault.StageAlign, fault.KindBadConfig, "duplicate representation %s", rep.ID)
		}
	}
	b.reps = append(b.reps, rep)
	return nil
}

// AddIssue records a recoverable problem for the manifest reader.
func (b *Builder) AddIssue(msg string) {
	if msg != "" {
		b.issues = append(b.issues, msg)
	}
}

// EntryFor builds a manifest entry for one representation segment. It is
// deterministic and does not touch the filesystem; the writer fills ContentHash
// and Status after the payload exists.
func (b *Builder) EntryFor(rep Representation, track *model.Track, startUS, endUS int64, requested int64, snap int64, switchable bool) (Entry, error) {
	if endUS <= startUS {
		return Entry{}, fault.New(fault.StageAlign, fault.KindZeroDuration,
			"segment [%d,%d) is empty", startUS, endUS).At(rep.TrackID, startUS)
	}
	nums, err := sampleNumbers(track, startUS, endUS)
	if err != nil {
		return Entry{}, err
	}
	id := model.SegmentID(rep.ID, rep.TrackID, startUS, endUS)
	if b.seen[id] {
		return Entry{}, fault.New(fault.StageAlign, fault.KindBadConfig,
			"duplicate segment id %s", id).WithSegment(id)
	}
	b.seen[id] = true
	rel := b.relPath(rep.ID, startUS, endUS)
	e := Entry{
		ID:               id,
		Representation:   rep.ID,
		Bitrate:          rep.Bitrate,
		TrackID:          rep.TrackID,
		Kind:             model.SegmentSimple,
		StartUS:          startUS,
		EndUS:            endUS,
		RequestedStartUS: requested,
		SnapOffsetUS:     snap,
		Switchable:       switchable,
		SampleNumbers:    nums,
		File:             rel,
		Status:           "pending",
	}
	return e, nil
}

// CompositeEntry builds the entry for a transition region. The composite is
// reported against the primary representation but names every source track.
func (b *Builder) CompositeEntry(rep Representation, track *model.Track, tr TransitionRecord, startUS, endUS int64) (Entry, error) {
	e, err := b.EntryFor(rep, track, startUS, endUS, startUS, 0, false)
	if err != nil {
		return Entry{}, err
	}
	e.Kind = model.SegmentComposite
	e.TransitionKind = tr.Kind
	e.SourceTrackIDs = append([]int(nil), tr.Sources...)
	b.seen[e.ID] = true
	return e, nil
}

// Append adds a completed entry. Entries are sorted by (start, track,
// representation, id) on Serialise so ordering cannot depend on the order in
// which workers finished.
func (b *Builder) Append(e Entry) { b.entries = append(b.entries, e) }

// Entries returns a copy of the accumulated entries in manifest order.
func (b *Builder) Entries() []Entry {
	out := append([]Entry(nil), b.entries...)
	sortEntries(out)
	return out
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, c := entries[i], entries[j]
		if a.StartUS != c.StartUS {
			return a.StartUS < c.StartUS
		}
		if a.TrackID != c.TrackID {
			return a.TrackID < c.TrackID
		}
		if a.Representation != c.Representation {
			return a.Representation < c.Representation
		}
		return a.ID < c.ID
	})
}

// Build assembles the manifest document.
func (b *Builder) Build(spanUS int64, transitions []TransitionRecord) *Manifest {
	entries := b.Entries()
	reps := append([]Representation(nil), b.reps...)
	sort.SliceStable(reps, func(i, j int) bool { return reps[i].ID < reps[j].ID })
	trs := append([]TransitionRecord(nil), transitions...)
	sort.SliceStable(trs, func(i, j int) bool {
		if trs[i].StartUS != trs[j].StartUS {
			return trs[i].StartUS < trs[j].StartUS
		}
		return trs[i].Representative < trs[j].Representative
	})
	return &Manifest{
		Version:         ManifestVersion,
		SegmentUS:       b.opts.SegmentUS,
		SpanUS:          spanUS,
		Representations: reps,
		Transitions:     trs,
		Entries:         entries,
		Issues:          append([]string(nil), b.issues...),
	}
}

// Write serialises the manifest to dir/manifest.json atomically.
func Write(m *Manifest, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fault.From(err, fault.StageWrite, fault.KindPathEscape)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fault.From(err, fault.StageWrite, fault.KindWrite)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "manifest.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fault.From(err, fault.StageWrite, fault.KindWrite)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fault.From(err, fault.StageWrite, fault.KindWrite)
	}
	return path, nil
}

// Read loads and validates a manifest from dir/manifest.json.
func Read(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.From(err, fault.StageVerify, fault.KindMissingIndex)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fault.From(err, fault.StageVerify, fault.KindCorruptSample)
	}
	if m.Version != ManifestVersion {
		return nil, fault.New(fault.StageVerify, fault.KindBadConfig,
			"manifest version %d unsupported (want %d)", m.Version, ManifestVersion)
	}
	return &m, nil
}

// VerifyHash recomputes the content hash of a written segment and compares it
// with the recorded value.
func VerifyHash(file, want string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fault.From(err, fault.StageVerify, fault.KindMissingIndex)
	}
	sum := sha256.Sum256(data)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if want != "" && got != want {
		return got, fault.New(fault.StageVerify, fault.KindCorruptSample,
			"hash mismatch: got %s want %s", got, want)
	}
	return got, nil
}

// relPath builds the stable output path for a segment. It is derived purely
// from the representation and boundary so re-runs overwrite the same file.
func (b *Builder) relPath(repID string, startUS, endUS int64) string {
	return filepath.Join(repID, fmt.Sprintf("%012d-%012d.%s", startUS, endUS, b.opts.Ext))
}

// AbsPath resolves a manifest entry's File against the manifest directory,
// refusing paths that escape it.
func AbsPath(dir, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fault.New(fault.StageWrite, fault.KindPathEscape, "segment path %q escapes output", rel)
	}
	return filepath.Join(dir, clean), nil
}

// sampleNumbers lists the 1-based sample numbers of track that begin inside
// [startUS, endUS). A boundary that falls inside a sample is included so the
// fragment records the snap it absorbed.
func sampleNumbers(track *model.Track, startUS, endUS int64) ([]int, error) {
	if track == nil {
		return nil, fault.New(fault.StageAlign, fault.KindBadConfig, "nil track")
	}
	var nums []int
	for _, s := range track.Samples {
		if s.PTSUS < startUS {
			continue
		}
		if s.PTSUS >= endUS {
			break
		}
		nums = append(nums, s.Number)
	}
	if len(nums) == 0 {
		return nil, fault.New(fault.StageAlign, fault.KindMissingIndex,
			"track %d has no samples in [%d,%d)", track.ID, startUS, endUS).At(track.ID, startUS)
	}
	return nums, nil
}

// PlanFromEntry reconstructs the cut points for a segment so the writer can
// fetch exactly the samples named in the manifest.
func PlanFromEntry(e Entry, track *model.Track) ([]model.Sample, error) {
	if track == nil {
		return nil, fault.New(fault.StageWrite, fault.KindBadConfig, "nil track").WithSegment(e.ID)
	}
	byNum := make(map[int]model.Sample, len(track.Samples))
	for _, s := range track.Samples {
		byNum[s.Number] = s
	}
	out := make([]model.Sample, 0, len(e.SampleNumbers))
	for _, n := range e.SampleNumbers {
		s, ok := byNum[n]
		if !ok {
			return nil, fault.New(fault.StageWrite, fault.KindMissingIndex,
				"track %d has no sample %d", track.ID, n).WithSegment(e.ID)
		}
		out = append(out, s)
	}
	return out, nil
}

// AlignEntries converts an aligned boundary list into per-representation
// manifest entries, making composites for any segment that intersects a
// transition region.
func (b *Builder) AlignEntries(a *align.Aligned, reps []align.Representation, tracks map[int]*model.Track, idx map[string]*index.TrackIndex, transitions []model.Transition) ([]Entry, []Entry, error) {
	var simple, composite []Entry
	for _, rep := range reps {
		track := tracks[rep.TrackID]
		if track == nil {
			return nil, nil, fault.New(fault.StageAlign, fault.KindBadConfig,
				"representation %s has no track %d", rep.ID, rep.TrackID)
		}
		mrep := Representation{ID: rep.ID, Bitrate: rep.Bitrate, TrackID: rep.TrackID, Codec: track.Codec, Timescale: track.Timescale}
		for i := 0; i+1 < len(a.Boundaries); i++ {
			from, to := a.Boundaries[i], a.Boundaries[i+1]
			if to.EffectiveUS <= from.EffectiveUS {
				continue
			}
			tr, overlaps := overlappingTransition(transitions, from.EffectiveUS, to.EffectiveUS)
			if overlaps {
				rec := TransitionRecord{
					StartUS:        tr.StartUS,
					EndUS:          tr.EndUS,
					Kind:           string(tr.Kind),
					Sources:        append([]int(nil), tr.Sources...),
					Representative: rep.ID,
				}
				e, err := b.CompositeEntry(mrep, track, rec, from.EffectiveUS, to.EffectiveUS)
				if err != nil {
					return nil, nil, err
				}
				composite = append(composite, e)
				continue
			}
			e, err := b.EntryFor(mrep, track, from.EffectiveUS, to.EffectiveUS,
				from.RequestedUS, from.RequestedUS-from.EffectiveUS, from.Switchable)
			if err != nil {
				return nil, nil, err
			}
			simple = append(simple, e)
		}
	}
	return simple, composite, nil
}

func overlappingTransition(trs []model.Transition, startUS, endUS int64) (model.Transition, bool) {
	for _, tr := range trs {
		if tr.StartUS < endUS && tr.EndUS > startUS {
			return tr, true
		}
	}
	return model.Transition{}, false
}
