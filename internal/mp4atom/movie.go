package mp4atom

import (
	"io"

	"mp4replay/internal/boxscan"
)

// Track bundles one trak's parsed table, samples and timeline.
type Track struct {
	Box      *boxscan.Box
	Table    *Table
	Samples  []Sample
	Timeline *Timeline
	Stbl     *boxscan.Box
}

// Handler returns the 4-char handler type, e.g. vide/soun.
func (tr *Track) Handler() string { return tr.Table.Handler }

// Movie is the single parsed result shared by seek/export/hash.
type Movie struct {
	File        *boxscan.File
	Movie       *MovieInfo
	Tracks      []*Track
	Moov        *boxscan.Box
	UnknownSkip int
}

// knownLeaf lists leaf boxes the parser understands, for unknown counting.
var knownLeaf = map[string]bool{
	"mvhd": true, "tkhd": true, "mdhd": true, "hdlr": true, "stsd": true,
	"stts": true, "ctts": true, "stss": true, "stsc": true, "stsz": true,
	"stco": true, "co64": true, "elst": true, "edts": true, "mdia": true,
	"minf": true, "stbl": true, "trak": true, "moov": true, "mdat": true,
	"ftyp": true, "free": true, "skip": true, "dref": true, "url": true,
	"vmhd": true, "smhd": true, "dinf": true, "nmhd": true,
}

// Open scans and fully parses an MP4 file. Sample bytes stay on disk.
func Open(path string) (*Movie, error) {
	f, err := boxscan.Open(path)
	if err != nil {
		return nil, err
	}
	if err := boxscan.MustContainMoov(f.Roots); err != nil {
		f.Close()
		return nil, err
	}
	moov := boxscan.FindFirst(f.Roots, "moov")
	mi, err := ParseMovie(f.ReaderAt(), moov)
	if err != nil {
		f.Close()
		return nil, err
	}
	m := &Movie{
		File:        f,
		Movie:       mi,
		Moov:        moov,
		UnknownSkip: boxscan.CountUnknown(f.Roots, knownLeaf),
	}
	ra := f.ReaderAt()
	for _, trak := range moov.Children {
		if trak.Type != "trak" {
			continue
		}
		tab, err := ParseTrak(ra, trak)
		if err != nil {
			f.Close()
			return nil, err
		}
		samples, err := BuildSamples(tab)
		if err != nil {
			f.Close()
			return nil, err
		}
		// The sum of stts deltas is the authoritative media duration. A v0
		// mdhd duration can be truncated by the same uint32 rollover the
		// samples exhibit, so prefer the reconstructed value.
		var sttsSum uint64
		for _, e := range tab.Stts {
			sttsSum += uint64(e.Count) * uint64(e.Delta)
		}
		if sttsSum > 0 {
			tab.MediaDur = sttsSum
		}
		tl, err := BuildTimeline(tab, mi.Timescale, samples)
		if err != nil {
			f.Close()
			return nil, err
		}
		stbl := boxscan.Path(trak, "mdia", "minf", "stbl")
		if err := tab.Validate(ra, stbl); err != nil {
			f.Close()
			return nil, err
		}
		m.Tracks = append(m.Tracks, &Track{
			Box: trak, Table: tab, Samples: samples,
			Timeline: tl, Stbl: stbl,
		})
	}
	return m, nil
}

// ReaderAt exposes the underlying read-only file for byte-level access.
func (m *Movie) ReaderAt() io.ReaderAt { return m.File.ReaderAt() }

// Close releases the file handle.
func (m *Movie) Close() error { return m.File.Close() }

// VideoTrack returns the first vide handler track, or nil.
func (m *Movie) VideoTrack() *Track {
	for _, tr := range m.Tracks {
		if tr.Handler() == "vide" {
			return tr
		}
	}
	return nil
}

// AudioTrack returns the first soun handler track, or nil.
func (m *Movie) AudioTrack() *Track {
	for _, tr := range m.Tracks {
		if tr.Handler() == "soun" {
			return tr
		}
	}
	return nil
}
