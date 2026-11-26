package fixture

import (
	"fmt"
	"sort"
)

// TrackKind labels a synthetic track.
type TrackKind string

const (
	KindVideo   TrackKind = "video"
	KindAudio   TrackKind = "audio"
	KindRemote  TrackKind = "remote_dub"
	KindOrphan  TrackKind = "cyclic"
)

// TrackSpec declares one track and its explicit synchronization dependencies.
type TrackSpec struct {
	ID           uint32
	Kind         TrackKind
	Timescale    uint32
	DependsOn    []uint32 // edges of the explicit track dependency graph
	SampleDur    uint32   // per-sample duration in track timescale
}

// InputFile is one delivered fixture file plus its expected parse outcome.
type InputFile struct {
	Name     string
	TrackID  uint32
	Sequence uint32
	Bytes    []byte
	// Truncated marks files whose box structure is intentionally damaged.
	Truncated bool
	// DuplicateOf names the earlier file whose bytes this file repeats.
	DuplicateOf string
}

// Scenario is the complete deterministic demo/test input set.
type Scenario struct {
	Tracks []TrackSpec
	Files  []InputFile
	// Counts asserted by the demo acceptance line.
	Generated int
}

// Track specs: audio and the remote dub explicitly synchronize to video;
// track 404 points at itself, which the graph must reject as a cycle.
func defaultTracks() []TrackSpec {
	return []TrackSpec{
		{ID: 101, Kind: KindVideo, Timescale: 90000, SampleDur: 3000},
		{ID: 201, Kind: KindAudio, Timescale: 48000, DependsOn: []uint32{101}, SampleDur: 1024},
		{ID: 301, Kind: KindRemote, Timescale: 48000, DependsOn: []uint32{101, 201}, SampleDur: 1024},
		{ID: 404, Kind: KindOrphan, Timescale: 90000, DependsOn: []uint32{404}, SampleDur: 3000},
	}
}

// BuildAllScenario constructs the "all profiles" fixture set: exactly 25
// input files, one duplicate, one truncated fragment and one cyclic track.
func BuildAllScenario() *Scenario {
	tracks := defaultTracks()
	spec := map[uint32]TrackSpec{101: tracks[0], 201: tracks[1], 301: tracks[2]}
	var files []InputFile

	// Video track: 12 healthy fragments with a mfhd gap (seq 5 missing)
	// and a tfdt rollback injected at sequence 8.
	videoSeqs := []uint32{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13}
	videoDTS := uint64(0)
	for i, seq := range videoSeqs {
		dts := videoDTS
		if seq == 8 {
			dts = videoDTS - 6000 // backwards jump relative to seq 7
		}
		raw := twoSampleFragment(spec[101], seq, dts, i == 0)
		files = append(files, InputFile{
			Name: fname(101, seq), TrackID: 101, Sequence: seq, Bytes: raw,
		})
		videoDTS += 6000 // two 3000-unit samples per fragment
	}

	// Audio track: 6 healthy fragments with a mfhd gap (seq 3 missing).
	audioSeqs := []uint32{1, 2, 4, 5, 6, 7}
	audioDTS := uint64(0)
	for i, seq := range audioSeqs {
		raw := twoSampleFragment(spec[201], seq, audioDTS, i == 0)
		files = append(files, InputFile{
			Name: fname(201, seq), TrackID: 201, Sequence: seq, Bytes: raw,
		})
		audioDTS += 2048
	}

	// Remote dub: 5 healthy fragments (seq 2 redelivered once as a duplicate,
	// seq 6 intentionally missing), so 5 unique + 1 redelivery + 1 truncation.
	dubSeqs := []uint32{1, 2, 3, 4, 5}
	dubDTS := uint64(0)
	for i, seq := range dubSeqs {
		raw := twoSampleFragment(spec[301], seq, dubDTS, i == 0)
		files = append(files, InputFile{
			Name: fname(301, seq), TrackID: 301, Sequence: seq, Bytes: raw,
		})
		if seq == 2 {
			files = append(files, InputFile{
				Name: fname(301, 20) /* redelivery slot */, TrackID: 301,
				Sequence: 2, Bytes: append([]byte(nil), raw...),
				DuplicateOf: fname(301, 2),
			})
		}
		dubDTS += 2048
	}

	// Truncated fragment: valid moof+mdat cut through the mdat payload.
	full := twoSampleFragment(spec[301], 99, dubDTS, false)
	cut := len(full) - 40
	files = append(files, InputFile{
		Name: fname(301, 99), TrackID: 301, Sequence: 99,
		Bytes: full[:cut], Truncated: true,
	})

	sort.SliceStable(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return &Scenario{Tracks: tracks, Files: files, Generated: len(files)}
}

// twoSampleFragment builds a fragment holding two deterministic samples; the
// first sample is a sync (keyframe) sample, the second is not.
func twoSampleFragment(t TrackSpec, seq uint32, dts uint64, forceSync bool) []byte {
	p1 := SamplePayload(t.ID, seq, 1, 64)
	p2 := SamplePayload(t.ID, seq, 2, 48)
	syncFirst := true
	samples := []SampleSpec{
		{Duration: t.SampleDur, Payload: p1, Sync: syncFirst || forceSync},
		{Duration: t.SampleDur, Payload: p2, Sync: false},
	}
	return BuildFragment(FragmentSpec{
		TrackID: t.ID, Sequence: seq, Tfdt: dts, Samples: samples,
	})
}

func fname(trackID, seq uint32) string {
	return fmt.Sprintf("t%03d_seq%03d.m4s", trackID, seq)
}
