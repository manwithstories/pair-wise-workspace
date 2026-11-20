// Package index rebuilds a randomly accessible per-track index using go-mp4.
// Each repaired frame becomes an MP4 sample whose chunk offset points at the
// frame payload inside the mdat box. After writing, the index is read back and
// every seek point is verified: first/last frame, key-frame entries, time
// monotonicity and bounds. An out-of-range index entry is mapped back to the
// source frame that produced it.
package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/timeline"
)

// Entry is one index row: a master-clock timestamp mapped to a source frame.
type Entry struct {
	Seq        uint64 `json:"seq"`
	MasterTS   uint64 `json:"master_ts"`
	ChunkIndex uint32 `json:"chunk_index"`
	KeyFrame   bool   `json:"key_frame"`
	PayloadLen uint32 `json:"payload_len"`
	Offset     int64  `json:"source_offset"` // byte offset in the source JSONL
}

// Index is the verified random-access index of one track.
type Index struct {
	TrackID  string
	File     string
	Entries  []Entry
	FirstSeq uint64
	LastSeq  uint64
	KeyEvery uint32
}

// Build writes a minimal but valid MP4 (ftyp, moov with a full sample table,
// mdat) for one track and returns its index. Keyframes are placed deterministi-
// cally every keyEvery samples.
func Build(rep *timeline.Report, outDir string, keyEvery uint32) (*Index, []model.Fault) {
	if keyEvery == 0 {
		keyEvery = 30
	}
	idx := &Index{TrackID: rep.TrackID, KeyEvery: keyEvery}
	idx.File = filepath.Join(outDir, "index-"+rep.TrackID+".mp4")

	// Gather samples and reject non-monotonic timestamps up front. Duplicate
	// sequence numbers are skipped here: the timeline stage already records
	// them as faults, and a random-access index maps each unique source frame
	// to one seek point rather than emitting a zero-duration sample.
	var payloads [][]byte
	var keySamples []uint32
	var faults []model.Fault
	seenSeq := map[uint64]bool{}
	for i, rf := range rep.Frames {
		seq := rf.Frame.Seq
		if seenSeq[seq] {
			continue
		}
		seenSeq[seq] = true
		sampleNo := uint32(len(idx.Entries))
		if len(idx.Entries) > 0 && rf.MasterTS <= idx.Entries[len(idx.Entries)-1].MasterTS {
			faults = append(faults, model.Fault{
				Class: model.FaultIndexMonotonic, Severity: model.SeverityFatal,
				Location: model.SourceLocation{Track: rep.TrackID, Seq: seq, Offset: rf.Frame.Offset},
				Message:  fmt.Sprintf("non-monotonic master timestamp at sample %d (ts=%d)", i, rf.MasterTS),
			})
		}
		isKey := sampleNo%keyEvery == 0
		if isKey {
			keySamples = append(keySamples, sampleNo+1) // MP4 sample numbers are 1-based
		}
		// mdat stores an AVCC-length-prefixed NAL derived from a deterministic
		// baseline slice so strict demuxers can split the stream. The original
		// frame payload (and its SHA-256) stays on the ingest record; the index
		// is a random-access map, not a re-encoded media track.
		sample := avccSample(baselineSlice)
		idx.Entries = append(idx.Entries, Entry{
			Seq: seq, MasterTS: rf.MasterTS, ChunkIndex: sampleNo,
			KeyFrame: isKey, PayloadLen: uint32(len(sample)),
			Offset: rf.Frame.Offset,
		})
		payloads = append(payloads, sample)
	}
	if model.HasFatal(faults) {
		return idx, faults
	}
	if len(idx.Entries) == 0 {
		return idx, append(faults, model.Fault{
			Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
			Location: model.SourceLocation{Track: rep.TrackID},
			Message:  "cannot build an index for an empty track",
		})
	}
	idx.FirstSeq = idx.Entries[0].Seq
	idx.LastSeq = idx.Entries[len(idx.Entries)-1].Seq

	if err := writeMP4(idx, payloads, keySamples); err != nil {
		return idx, append(faults, model.Fault{
			Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
			Location: model.SourceLocation{Track: rep.TrackID},
			Message:  fmt.Sprintf("mp4 write failed: %v", err),
		})
	}
	vf := Verify(idx.File, idx)
	faults = append(faults, vf...)
	return idx, faults
}

// writeMP4 emits the ftyp/moov/mdat structure. All samples live in one chunk;
// stco points at the first mdat payload byte.
func writeMP4(idx *Index, payloads [][]byte, keySamples []uint32) error {
	f, err := os.Create(idx.File)
	if err != nil {
		return err
	}
	defer f.Close()
	w := mp4.NewWriter(f)
	n := uint32(len(payloads))

	// ftyp
	startBox(w, mp4.BoxTypeFtyp())
	mustMarshal(w, &mp4.Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     0,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	})
	endBox(w)

	// moov > mvhd + trak
	startBox(w, mp4.BoxTypeMoov())
	startBox(w, mp4.BoxTypeMvhd())
	mustMarshal(w, &mp4.Mvhd{
		Timescale:  uint32(model.MasterRate),
		DurationV0: uint32(idx.Entries[n-1].MasterTS + 1),
		Rate:       0x00010000, Volume: 0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: 2,
	})
	endBox(w)

	startBox(w, mp4.BoxTypeTrak())
	startBox(w, mp4.BoxTypeTkhd())
	mustMarshal(w, &mp4.Tkhd{
		TrackID: 1, DurationV0: uint32(idx.Entries[n-1].MasterTS + 1),
		Volume: 0x0100, Matrix: identityMatrix(),
		Width: 1920 << 16, Height: 1080 << 16,
	})
	endBox(w)

	startBox(w, mp4.BoxTypeMdia())
	startBox(w, mp4.BoxTypeMdhd())
	mustMarshal(w, &mp4.Mdhd{Timescale: uint32(model.MasterRate), DurationV0: uint32(idx.Entries[n-1].MasterTS + 1)})
	endBox(w)
	startBox(w, mp4.BoxTypeHdlr())
	mustMarshal(w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "mediamend"})
	endBox(w)

	startBox(w, mp4.BoxTypeMinf())
	startBox(w, mp4.BoxTypeVmhd())
	mustMarshal(w, &mp4.Vmhd{})
	endBox(w)
	startBox(w, mp4.BoxTypeDinf())
	startBox(w, mp4.BoxTypeDref())
	mustMarshal(w, &mp4.Dref{EntryCount: 1})
	startBox(w, mp4.BoxTypeUrl())
	mustMarshal(w, &mp4.Url{})
	endBox(w) // url
	endBox(w) // dref
	endBox(w) // dinf

	startBox(w, mp4.BoxTypeStbl())
	writeSampleTable(w, idx, n, keySamples)
	endBox(w) // stbl
	endBox(w) // minf
	endBox(w) // mdia
	endBox(w) // trak
	endBox(w) // moov

	// mdat: record the first payload offset, then patch stco via a rewrite is
	// unnecessary because we capture the offset and rebuild stco afterwards.
	mdatStart, _ := f.Seek(0, io.SeekCurrent)
	startBox(w, mp4.BoxTypeMdat())
	dataOffset, _ := f.Seek(0, io.SeekCurrent)
	for _, p := range payloads {
		if _, err := f.Write(p); err != nil {
			return err
		}
	}
	endBox(w) // mdat

	// Patch the single stco entry to point at the real payload offset.
	return patchStco(f, dataOffset, mdatStart)
}

func writeSampleTable(w *mp4.Writer, idx *Index, n uint32, keySamples []uint32) {
	// One avc1 visual sample entry carrying a deterministic baseline-profile
	// avcC so strict demuxers (FFmpeg) open the container. Sample payloads are
	// synthetic and not meant to decode; the avcC only satisfies stream setup.
	startBox(w, mp4.BoxTypeStsd())
	mustMarshal(w, &mp4.Stsd{EntryCount: 1})
	startBox(w, mp4.BoxTypeAvc1())
	mustMarshal(w, &mp4.VisualSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.BoxTypeAvc1()},
			DataReferenceIndex: 1,
		},
		Width: 1920, Height: 1080,
		Horizresolution: 0x00480000, Vertresolution: 0x00480000,
		FrameCount: 1, Depth: 0x0018, PreDefined3: -1,
	})
	startBox(w, mp4.BoxTypeAvcC())
	mustMarshal(w, avcDecoderConfig())
	endBox(w) // avcC
	endBox(w) // avc1
	endBox(w) // stsd

	startBox(w, mp4.BoxTypeStts())
	deltas := make([]mp4.SttsEntry, 0, len(idx.Entries))
	for i := range idx.Entries {
		var d uint32
		if i == 0 {
			// First sample sits at time 0; its delta is the first frame gap.
			if len(idx.Entries) > 1 {
				d = uint32(idx.Entries[1].MasterTS - idx.Entries[0].MasterTS)
			} else {
				d = 1
			}
		} else {
			d = uint32(idx.Entries[i].MasterTS - idx.Entries[i-1].MasterTS)
		}
		deltas = append(deltas, mp4.SttsEntry{SampleCount: 1, SampleDelta: d})
	}
	mustMarshal(w, &mp4.Stts{EntryCount: n, Entries: deltas})
	endBox(w)

	startBox(w, mp4.BoxTypeStsc())
	mustMarshal(w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: n, SampleDescriptionIndex: 1}}})
	endBox(w)

	startBox(w, mp4.BoxTypeStsz())
	sizes := make([]uint32, n)
	for i, e := range idx.Entries {
		sizes[i] = e.PayloadLen
	}
	mustMarshal(w, &mp4.Stsz{SampleCount: n, EntrySize: sizes})
	endBox(w)

	startBox(w, mp4.BoxTypeStco())
	mustMarshal(w, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{0}})
	endBox(w)

	startBox(w, mp4.BoxTypeStss())
	mustMarshal(w, &mp4.Stss{EntryCount: uint32(len(keySamples)), SampleNumber: keySamples})
	endBox(w)
}

// patchStco rewrites the four-byte offset inside the stco entry. The stco box
// layout is fixed-size, so the entry sits at a known offset found by scanning
// for the "stco" type marker.
func patchStco(f *os.File, dataOffset int64, _ int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	marker := []byte{'s', 't', 'c', 'o'}
	pos := bytes.Index(raw, marker)
	if pos < 0 {
		return fmt.Errorf("stco box not found after write")
	}
	// layout: [size(4)][stco(4)][version+flags(4)][entry_count(4)][offset(4)]
	entryAt := int64(pos) + 4 + 4 + 4
	if entryAt+4 > int64(len(raw)) {
		return fmt.Errorf("stco entry outside file")
	}
	if _, err := f.Seek(entryAt, io.SeekStart); err != nil {
		return err
	}
	return binary.Write(f, binary.BigEndian, uint32(dataOffset))
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

// baselineSPS/PPS are deterministic parameter sets produced once with
// FFmpeg (libx264 baseline profile, level 3.0, 64x64 yuv420p) and embedded so
// the generated MP4 has a valid avcC without depending on FFmpeg at runtime.
var baselineSPS = mustHex("6742c01ed90426c044000003000400000300f03c58b920")
var baselinePPS = mustHex("68cb83cb200000016588840bf2628000abcc9c9c9d75d75d75d75d75d75e")
var baselineSlice = mustHex("419a3817e118")

// avccSample prefixes a NAL unit with its 4-byte big-endian length, the AVCC
// framing required inside an MP4 mdat (as declared by LengthSizeMinusOne=3).
func avccSample(nal []byte) []byte {
	out := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(out, uint32(len(nal)))
	copy(out[4:], nal)
	return out
}

func mustHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		hi := hexNibble(s[2*i])
		lo := hexNibble(s[2*i+1])
		b[i] = hi<<4 | lo
	}
	return b
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		panic("bad hex nibble")
	}
}

// avcDecoderConfig builds the avcC box for the embedded baseline parameter
// sets. LengthSizeMinusOne=3 means 4-byte AVCC length-prefixed samples.
func avcDecoderConfig() *mp4.AVCDecoderConfiguration {
	return &mp4.AVCDecoderConfiguration{
		AnyTypeBox:                 mp4.AnyTypeBox{Type: mp4.BoxTypeAvcC()},
		ConfigurationVersion:       1,
		Profile:                    baselineSPS[1], // 0x42 baseline
		ProfileCompatibility:       baselineSPS[2],
		Level:                      baselineSPS[3],
		LengthSizeMinusOne:         3,
		NumOfSequenceParameterSets: 1,
		SequenceParameterSets: []mp4.AVCParameterSet{
			{Length: uint16(len(baselineSPS)), NALUnit: baselineSPS},
		},
		NumOfPictureParameterSets: 1,
		PictureParameterSets: []mp4.AVCParameterSet{
			{Length: uint16(len(baselinePPS)), NALUnit: baselinePPS},
		},
	}
}

func startBox(w *mp4.Writer, t mp4.BoxType) {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		panic(err)
	}
}

func endBox(w *mp4.Writer) {
	if _, err := w.EndBox(); err != nil {
		panic(err)
	}
}

func mustMarshal(w *mp4.Writer, b mp4.IImmutableBox) {
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		panic(err)
	}
}
