package probe

import (
	"bytes"
	"fmt"
	"math"
	"sort"

	"github.com/abema/go-mp4"

	"mediacheck/internal/clock"
	"mediacheck/internal/fault"
)

type sampleTable struct {
	sizes      []uint32 // nil when constSize is used
	constSize  uint32
	count      uint64
	stts       []timeRun
	stsc       []stscRun
	offsets    []uint64
	keySamples []uint32 // 1-based sample numbers, sorted
	ctts       []cttsRun
	hasCTTS    bool
	hasSTSS    bool
}

type timeRun struct {
	count uint32
	delta uint32
}

type stscRun struct {
	firstChunk      uint32
	samplesPerChunk uint32
}

type cttsRun struct {
	count  uint32
	offset int64
}

func (t *Track) addIssue(f *fault.Fault) {
	if len(t.Issues) < t.cfg.MaxIssues {
		t.Issues = append(t.Issues, f)
	}
}

// probeFile parses one MP4/MOV input. The returned File does not hold the
// file descriptor; sample payload validation reopens the path on demand.
func probeFile(path string, cfg Config) (*File, error) {
	cfg = cfg.Normalize()
	c, err := openContainer(path, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	f := &File{
		Path:            path,
		Size:            c.size,
		Boxes:           c.boxes,
		MOOFCount:       c.moofCount,
		Truncated:       c.truncated,
		FirstUnreadable: -1,
		DiscardedBytes:  c.discarded,
		Fragmented:      c.moofCount > 0,
	}
	f.Issues = append(f.Issues, c.faults...)
	if c.moovBox == nil {
		f.Issues = append(f.Issues, fault.At(fault.CodeIndexMissing, fault.SeverityPartial, "probe", c.moovStart,
			"no moov box; the sample index is unavailable"))
		return f, nil
	}
	if c.moofCount > 0 {
		f.Issues = append(f.Issues, fault.At(fault.CodeUnsupported, fault.SeverityWarning, "probe", 0,
			"fragmented input: only the moov sample index is analyzed, moof fragments are not walked"))
	}
	c.parseMoov(f, cfg)
	if c.truncated {
		f.FirstUnreadable = c.firstUnread
	}
	for _, t := range f.Tracks {
		if off := t.firstUnreadable(); off >= 0 && (f.FirstUnreadable < 0 || off < f.FirstUnreadable) {
			f.FirstUnreadable = off
		}
	}
	if len(f.Tracks) == 0 {
		f.Issues = append(f.Issues, fault.At(fault.CodeStructure, fault.SeverityPartial, "probe", c.moovStart,
			"moov contains no usable trak boxes"))
	}
	return f, nil
}

func (c *container) parseMoov(f *File, cfg Config) {
	kids := c.children(c.moovPayload, c.moovStart, "moov")
	movieTimescale := uint32(0)
	if mv, ok := c.findChild(kids, "mvhd"); ok {
		var box mp4.Mvhd
		if err := c.decode(mv, &box); err == nil {
			movieTimescale = box.Timescale
		}
	}
	for _, kid := range kids {
		if kid.Type != "trak" {
			continue
		}
		if len(f.Tracks) >= 32 {
			f.Issues = append(f.Issues, fault.At(fault.CodeStructure, fault.SeverityPartial, "moov", kid.Offset,
				"track limit reached"))
			break
		}
		if t := c.parseTrak(kid, cfg, movieTimescale); t != nil {
			f.Tracks = append(f.Tracks, t)
		}
	}
}

func (c *container) parseTrak(trak childBox, cfg Config, movieTimescale uint32) *Track {
	t := &Track{
		Codec:            "unknown",
		Kind:             KindOther,
		UnreadableOffset: -1,
		FirstSample:      -1,
		DataEnd:          -1,
		cfg:              cfg,
		mdats:            c.mdats,
		size:             c.size,
		movieTimescale:   movieTimescale,
	}
	kids := c.children(trak.Payload, trak.payloadStart(), "trak")
	c.parseTrakFields(t, kids, trak.Offset)
	if t.ID == 0 {
		return nil
	}
	return t
}

func (c *container) parseTrakFields(t *Track, kids []childBox, trakOffset int64) {
	if tk, ok := c.findChild(kids, "tkhd"); ok {
		var box mp4.Tkhd
		if err := c.decode(tk, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "tkhd", 0, tk.Offset, err.Error()))
		} else {
			t.ID = box.TrackID
		}
	}
	mdia, ok := c.findChild(kids, "mdia")
	if !ok {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "trak", t.ID, trakOffset, "missing mdia"))
		return
	}
	mkids := c.children(mdia.Payload, mdia.payloadStart(), "mdia")
	if mh, ok := c.findChild(mkids, "mdhd"); ok {
		var box mp4.Mdhd
		if err := c.decode(mh, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "mdhd", t.ID, mh.Offset, err.Error()))
		} else {
			t.Timescale = box.Timescale
			t.DurationMS = clock.TicksToMS(box.GetDuration(), box.Timescale)
			if box.Timescale == 0 {
				t.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityFatal, "mdhd", t.ID, mh.Offset, "zero timescale"))
			}
		}
	}
	if hd, ok := c.findChild(mkids, "hdlr"); ok {
		var box mp4.Hdlr
		if err := c.decode(hd, &box); err == nil {
			t.Handler = string(box.HandlerType[:])
			switch t.Handler {
			case "vide":
				t.Kind = KindVideo
			case "soun":
				t.Kind = KindAudio
			default:
				t.Kind = KindOther
			}
		}
	}
	c.parseEditList(t, kids)

	minf, ok := c.findChild(mkids, "minf")
	if !ok {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "minf", t.ID, mdia.Offset, "missing minf"))
		return
	}
	nkids := c.children(minf.Payload, minf.payloadStart(), "minf")
	stbl, ok := c.findChild(nkids, "stbl")
	if !ok {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stbl", t.ID, minf.Offset, "missing stbl"))
		return
	}
	skids := c.children(stbl.Payload, stbl.payloadStart(), "stbl")
	c.parseStsd(t, skids)
	c.parseSampleTable(t, skids)
}

// parseStsd identifies the first sample entry's four-character codec.
func (c *container) parseStsd(t *Track, kids []childBox) {
	stsd, ok := c.findChild(kids, "stsd")
	if !ok {
		return
	}
	if len(stsd.Payload) < 8 {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stsd", t.ID, stsd.Offset, "short stsd payload"))
		return
	}
	entries := c.children(stsd.Payload[8:], stsd.payloadStart()+8, "stsd")
	if len(entries) == 0 {
		return
	}
	t.Codec = codecName(entries[0].Type, t.Kind)
}

func codecName(fourcc string, kind Kind) string {
	switch fourcc {
	case "avc1", "avc3":
		return "h264"
	case "hvc1", "hev1":
		return "h265"
	case "av01":
		return "av1"
	case "vp09":
		return "vp9"
	case "mp4v":
		return "mpeg4"
	case "mp4a":
		return "aac"
	case "Opus":
		return "opus"
	case "alac":
		return "alac"
	case "ac-3":
		return "ac3"
	case "ec-3":
		return "eac3"
	case "encv", "enca":
		return "encrypted"
	}
	if fourcc != "" {
		return fourcc
	}
	return string(kind)
}

func (c *container) decode(b childBox, dst mp4.IBox) error {
	if int64(len(b.Payload)) > c.maxDecode {
		return fmt.Errorf("box %q payload %d exceeds decode cap %d", b.Type, len(b.Payload), c.maxDecode)
	}
	if _, err := mp4.Unmarshal(bytes.NewReader(b.Payload), uint64(len(b.Payload)), dst, mp4.Context{}); err != nil {
		return fmt.Errorf("decode %s: %w", b.Type, err)
	}
	return nil
}

// parseSampleTable fills the track's timeline description. Missing boxes are
// classified, not fatal: whatever can be walked is still exposed.
func (c *container) parseSampleTable(t *Track, kids []childBox) {
	st := &sampleTable{}
	t.table = st

	if b, ok := c.findChild(kids, "stts"); ok {
		var box mp4.Stts
		if err := c.decode(b, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stts", t.ID, b.Offset, err.Error()))
		} else {
			st.stts = make([]timeRun, 0, len(box.Entries))
			for _, e := range box.Entries {
				st.stts = append(st.stts, timeRun{count: e.SampleCount, delta: e.SampleDelta})
			}
			t.Index.TimeToSample = true
		}
	} else {
		t.addIssue(fault.ForTrack(fault.CodeIndexMissing, fault.SeverityPartial, "stts", t.ID, t.DataEnd,
			"missing stts; the timeline cannot be reconstructed"))
	}

	if b, ok := c.findChild(kids, "stsz"); ok {
		var box mp4.Stsz
		if err := c.decode(b, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stsz", t.ID, b.Offset, err.Error()))
		} else {
			st.count = uint64(box.SampleCount)
			st.constSize = box.SampleSize
			if box.SampleSize == 0 {
				st.sizes = box.EntrySize
				if uint64(len(box.EntrySize)) < st.count {
					t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "stsz", t.ID, b.Offset,
						fmt.Sprintf("stsz declares %d samples but carries %d entries", st.count, len(box.EntrySize))))
					t.Index.Rebuilt = true
				}
				st.count = min64(st.count, uint64(len(st.sizes)))
			}
			t.Index.SampleSizes = true
			t.Declared = uint64(box.SampleCount)
		}
	} else {
		t.addIssue(fault.ForTrack(fault.CodeIndexMissing, fault.SeverityPartial, "stsz", t.ID, t.DataEnd,
			"missing stsz; sample sizes are unknown"))
	}

	if b, ok := c.findChild(kids, "stsc"); ok {
		var box mp4.Stsc
		if err := c.decode(b, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stsc", t.ID, b.Offset, err.Error()))
		} else {
			st.stsc = make([]stscRun, 0, len(box.Entries))
			lastFirst := uint32(0)
			bad := false
			for _, e := range box.Entries {
				if e.FirstChunk == 0 || e.FirstChunk <= lastFirst {
					bad = true
					break
				}
				lastFirst = e.FirstChunk
				st.stsc = append(st.stsc, stscRun{firstChunk: e.FirstChunk, samplesPerChunk: e.SamplesPerChunk})
			}
			if bad {
				t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "stsc", t.ID, b.Offset,
					"stsc first_chunk values are not strictly increasing"))
				st.stsc = nil
			} else {
				t.Index.SampleToChunk = true
			}
		}
	} else if st.count > 0 {
		t.addIssue(fault.ForTrack(fault.CodeIndexMissing, fault.SeverityPartial, "stsc", t.ID, t.DataEnd,
			"missing stsc; chunk grouping inferred when possible"))
	}

	c.parseChunkOffsets(t, kids, st)
	c.parseSyncSamples(t, kids, st)
	c.parseComposition(t, kids, st)

	if len(st.stsc) == 0 && len(st.offsets) > 0 && st.count > 0 {
		// One chunk holding every sample is the only safe inference.
		if len(st.offsets) == 1 {
			st.stsc = []stscRun{{firstChunk: 1, samplesPerChunk: uint32(min64(st.count, math.MaxUint32))}}
			if st.count > math.MaxUint32 {
				st.stsc = nil
			}
			t.Index.SampleToChunk = st.stsc != nil
			t.Index.Rebuilt = true
		}
	}

	t.Index.Complete = t.Index.SampleSizes && t.Index.SampleToChunk && t.Index.ChunkOffsets && t.Index.TimeToSample
	if t.Index.Rebuilt {
		t.Index.Complete = false
	}
	if !t.Index.Complete {
		t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "index", t.ID, t.DataEnd,
			"sample index is missing or incomplete; usable range may be smaller"))
	}

	c.walkTimeline(t, st)
}

func (c *container) parseChunkOffsets(t *Track, kids []childBox, st *sampleTable) {
	if b, ok := c.findChild(kids, "stco"); ok {
		var box mp4.Stco
		if err := c.decode(b, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stco", t.ID, b.Offset, err.Error()))
		} else {
			st.offsets = make([]uint64, 0, len(box.ChunkOffset))
			for _, o := range box.ChunkOffset {
				st.offsets = append(st.offsets, uint64(o))
			}
			t.Index.ChunkOffsets = true
			if uint64(len(st.offsets)) < st.countChunks() {
				t.Index.Rebuilt = true
			}
		}
		return
	}
	if b, ok := c.findChild(kids, "co64"); ok {
		var box mp4.Co64
		if err := c.decode(b, &box); err != nil {
			t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "co64", t.ID, b.Offset, err.Error()))
		} else {
			st.offsets = append(st.offsets, box.ChunkOffset...)
			t.Index.ChunkOffsets = true
		}
		return
	}
	// Rebuild offsets from the mdat bounds when the layout is unambiguous.
	if rebuilt, ok := rebuildOffsets(st, c.mdats, c.size); ok {
		st.offsets = rebuilt
		t.Index.ChunkOffsets = true
		t.Index.Rebuilt = true
		t.addIssue(fault.ForTrack(fault.CodeIndexMissing, fault.SeverityPartial, "stco", t.ID, -1,
			"chunk offsets rebuilt from mdat bounds and sample sizes"))
		return
	}
	t.addIssue(fault.ForTrack(fault.CodeIndexMissing, fault.SeverityPartial, "stco", t.ID, t.DataEnd,
		"missing stco/co64; sample byte ranges cannot be confirmed"))
}

// countChunks derives the number of chunks referenced by stsc and stsz.
func (st *sampleTable) countChunks() uint64 {
	if len(st.stsc) == 0 {
		return 0
	}
	remaining := st.count
	for i, run := range st.stsc {
		first := uint64(run.firstChunk)
		var nextFirst uint64
		if i+1 < len(st.stsc) {
			nextFirst = uint64(st.stsc[i+1].firstChunk)
		}
		if nextFirst == 0 {
			// Last run: chunks needed to cover the remaining samples.
			if run.samplesPerChunk == 0 {
				return 0
			}
			need := remaining / uint64(run.samplesPerChunk)
			if remaining%uint64(run.samplesPerChunk) != 0 {
				need++
			}
			if first == 0 {
				return 0
			}
			return first - 1 + need
		}
		if nextFirst <= first || run.samplesPerChunk == 0 {
			return 0
		}
		chunks := nextFirst - first
		consumed := chunks * uint64(run.samplesPerChunk)
		if consumed > remaining {
			return 0
		}
		remaining -= consumed
	}
	// Trailing stsc runs without a following boundary never describe chunks.
	return 0
}

// rebuildOffsets places chunks contiguously from the first mdat payload. It
// only succeeds for a single mdat and a resolvable chunk plan; anything else
// is left unconfirmed rather than guessed.
func rebuildOffsets(st *sampleTable, mdats []Bounds, filesize int64) ([]uint64, bool) {
	if len(mdats) != 1 || len(st.stsc) == 0 || st.count == 0 {
		return nil, false
	}
	chunks := st.countChunks()
	if chunks == 0 || chunks > 1<<20 {
		return nil, false
	}
	plan := make([]uint32, 0, chunks)
	remaining := st.count
	for i, run := range st.stsc {
		first := uint64(run.firstChunk)
		var nextFirst uint64
		if i+1 < len(st.stsc) {
			nextFirst = uint64(st.stsc[i+1].firstChunk)
		} else {
			nextFirst = chunks + 1
		}
		if nextFirst <= first {
			return nil, false
		}
		for c := first; c < nextFirst; c++ {
			n := uint64(run.samplesPerChunk)
			if n > remaining {
				n = remaining
			}
			if n == 0 {
				return nil, false
			}
			plan = append(plan, uint32(n))
			remaining -= n
		}
	}
	if remaining != 0 {
		return nil, false
	}
	offsets := make([]uint64, 0, len(plan))
	var at uint64
	sampleIdx := uint64(0)
	for _, n := range plan {
		offsets = append(offsets, uint64(mdats[0].Start)+at)
		for i := uint32(0); i < n; i++ {
			if sampleIdx >= uint64(len(st.sizes)) && st.constSize == 0 {
				return nil, false
			}
			sz := uint64(st.constSize)
			if st.sizes != nil {
				sz = uint64(st.sizes[sampleIdx])
			}
			if at > uint64(filesize) || sz > uint64(filesize)-at {
				return nil, false
			}
			at += sz
			sampleIdx++
		}
	}
	if uint64(mdats[0].Start)+at > uint64(mdats[0].End) {
		return nil, false
	}
	return offsets, true
}

func (c *container) parseSyncSamples(t *Track, kids []childBox, st *sampleTable) {
	b, ok := c.findChild(kids, "stss")
	if !ok {
		// Without stss every sample is a sync sample (ISO/IEC 14496-12).
		return
	}
	var box mp4.Stss
	if err := c.decode(b, &box); err != nil {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "stss", t.ID, b.Offset, err.Error()))
		return
	}
	st.keySamples = make([]uint32, 0, len(box.SampleNumber))
	for _, n := range box.SampleNumber {
		st.keySamples = append(st.keySamples, n)
	}
	sort.Slice(st.keySamples, func(i, j int) bool { return st.keySamples[i] < st.keySamples[j] })
	st.hasSTSS = true
	t.Index.SyncSamples = true
	if len(st.keySamples) > 0 && st.keySamples[len(st.keySamples)-1] > uint32(min64(st.count, math.MaxUint32)) {
		t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "stss", t.ID, b.Offset,
			"stss references samples beyond stsz"))
	}
}

func (c *container) parseComposition(t *Track, kids []childBox, st *sampleTable) {
	b, ok := c.findChild(kids, "ctts")
	if !ok {
		return
	}
	var box mp4.Ctts
	if err := c.decode(b, &box); err != nil {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "ctts", t.ID, b.Offset, err.Error()))
		return
	}
	st.hasCTTS = true
	t.Index.Composition = true
	for _, e := range box.Entries {
		var off int64
		if box.GetVersion() == 0 {
			off = int64(e.SampleOffsetV0)
		} else {
			off = int64(e.SampleOffsetV1)
		}
		st.ctts = append(st.ctts, cttsRun{count: e.SampleCount, offset: off})
	}
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
