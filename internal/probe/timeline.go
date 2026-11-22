package probe

import (
	"fmt"
	"math"

	"github.com/abema/go-mp4"

	"mediacheck/internal/clock"
	"mediacheck/internal/fault"
)

// Row is one sample-table row with absolute coordinates.
type Row struct {
	Index      uint64
	Offset     int64
	Size       uint32
	DTS        uint64
	CTS        int64
	DurTicks   uint32
	DTSMS      int64
	CTSMS      int64
	EndDTSMS   int64
	Keyframe   bool
	Segment    int
	InBounds   bool
	InMdat     bool
	OutOfOrder bool
}

// Cursor walks a track's samples in DTS order without storing per-sample state.
type Cursor struct {
	t        *Track
	st       *sampleTable
	index    uint64
	chunk    int
	inChunk  uint32
	spc      uint32
	chunkOff uint64
	chunkPos uint64
	nextOff  int64
	dts      uint64
	cts      int64
	sttsRun  int
	sttsLeft uint32
	cttsRun  int
	cttsLeft uint32
	keyIdx   int
	done     bool
	err      error
}

// Cursor creates a bounded iterator over the track's sample rows.
func (t *Track) Cursor() *Cursor {
	if t == nil || t.table == nil || t.Timescale == 0 {
		return &Cursor{done: true}
	}
	c := &Cursor{t: t, st: t.table}
	if len(c.st.stts) > 0 {
		c.sttsLeft = c.st.stts[0].count
	}
	if len(c.st.ctts) > 0 {
		c.cttsLeft = c.st.ctts[0].count
	}
	return c
}

// Next returns the next row and whether one is available.
func (c *Cursor) Next() (Row, bool) {
	if c.done || c.err != nil {
		return Row{}, false
	}
	st := c.st
	if c.index >= st.count || c.index >= c.t.cfg.MaxSamples {
		c.done = true
		return Row{}, false
	}
	if c.index >= st.count {
		c.done = true
		return Row{}, false
	}
	for c.sttsLeft == 0 {
		c.sttsRun++
		if c.sttsRun >= len(st.stts) {
			c.done = true
			return Row{}, false
		}
		c.sttsLeft = st.stts[c.sttsRun].count
	}
	// Advance to the next chunk when the current one is exhausted.
	if c.spc == 0 || c.inChunk >= c.spc {
		if !c.advanceChunk() {
			return Row{}, false
		}
	}
	size := st.constSize
	if st.sizes != nil {
		size = st.sizes[c.index]
	}
	off := int64(c.chunkOff) + int64(c.chunkPos)
	delta := st.stts[c.sttsRun].delta
	row := Row{
		Index:    c.index,
		Offset:   off,
		Size:     size,
		DTS:      c.dts,
		CTS:      c.cts,
		DurTicks: delta,
		Keyframe: c.isKey(),
	}
	row.DTSMS = clock.AddMS(clock.TicksToMS(row.DTS, c.t.Timescale), c.t.StartMS)
	if row.CTS < 0 {
		row.CTSMS = clock.AddMS(-clock.TicksToMS(uint64(-row.CTS), c.t.Timescale), c.t.StartMS)
	} else {
		row.CTSMS = clock.AddMS(clock.TicksToMS(uint64(row.CTS), c.t.Timescale), c.t.StartMS)
	}
	row.EndDTSMS = clock.AddMS(clock.TicksToMS(row.DTS+uint64(delta), c.t.Timescale), c.t.StartMS)
	if row.EndDTSMS <= row.DTSMS {
		// Sub-millisecond samples still occupy at least one millisecond of
		// presentation so coverage checks cannot see a zero-width gap.
		row.EndDTSMS = clock.AddMS(row.DTSMS, 1)
	}
	row.Segment = clock.SegmentIndex(row.DTSMS, c.t.cfg.SegmentMS)
	if c.nextOff >= 0 && off < c.nextOff {
		row.OutOfOrder = true
	}
	if off >= 0 {
		c.nextOff = off + int64(size)
	}
	row.InBounds, row.InMdat = c.t.rangeOK(off, int64(size))

	// Advance state.
	c.dts += uint64(st.stts[c.sttsRun].delta)
	c.sttsLeft--
	if st.hasCTTS {
		for c.cttsLeft == 0 {
			c.cttsRun++
			if c.cttsRun >= len(st.ctts) {
				c.ttsDone()
				break
			}
			c.cttsLeft = st.ctts[c.cttsRun].count
		}
		if c.cttsRun < len(st.ctts) {
			c.cts = int64(c.dts) + st.ctts[c.cttsRun].offset
			c.cttsLeft--
		} else {
			c.cts = int64(c.dts)
		}
	} else {
		c.cts = int64(c.dts)
	}
	c.chunkPos += uint64(size)
	c.inChunk++
	c.index++
	if c.index >= st.count {
		c.done = true
	}
	return row, true
}

func (c *Cursor) ttsDone() { c.cttsRun = len(c.st.ctts) }

func (c *Cursor) advanceChunk() bool {
	if c.chunk >= len(c.st.offsets) {
		c.t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "cursor", c.t.ID,
			c.t.DataEnd, fmt.Sprintf("chunk offset table ends after %d chunks", c.chunk)))
		c.done = true
		return false
	}
	c.chunkOff = c.st.offsets[c.chunk]
	c.chunkPos = 0
	c.inChunk = 0
	c.spc = c.samplesPerChunk(uint32(c.chunk + 1))
	if c.spc == 0 {
		c.t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "cursor", c.t.ID,
			c.t.DataEnd, "stsc yields zero samples for a chunk"))
		c.done = true
		return false
	}
	c.chunk++
	return true
}

func (c *Cursor) samplesPerChunk(chunkNumber uint32) uint32 {
	if len(c.st.stsc) == 0 {
		return 0
	}
	spc := c.st.stsc[0].samplesPerChunk
	for _, run := range c.st.stsc {
		if run.firstChunk > chunkNumber {
			break
		}
		spc = run.samplesPerChunk
	}
	return spc
}

func (c *Cursor) isKey() bool {
	n := uint32(c.index + 1)
	if !c.st.hasSTSS {
		return true
	}
	for c.keyIdx < len(c.st.keySamples) && c.st.keySamples[c.keyIdx] < n {
		c.keyIdx++
	}
	return c.keyIdx < len(c.st.keySamples) && c.st.keySamples[c.keyIdx] == n
}

// rangeOK validates a sample against file and mdat bounds.
func (t *Track) rangeOK(off, size int64) (inBounds, inMdat bool) {
	if off < 0 || size < 0 || off > t.size || size > t.size-off {
		return false, false
	}
	for _, m := range t.mdats {
		if off >= m.Start && off+size <= m.End {
			return true, true
		}
	}
	return true, false
}

// FirstUnreadable returns the smallest structural damage offset, or -1.
func (t *Track) FirstUnreadable() int64 { return t.firstUnreadable() }

func (t *Track) firstUnreadable() int64 {
	off := t.UnreadableOffset
	for _, f := range t.Issues {
		if f.Offset >= 0 && (off < 0 || f.Offset < off) {
			off = f.Offset
		}
	}
	return off
}

// KeyTimes returns keyframe DTS values in milliseconds (bounded copy).
func (t *Track) KeyTimes() []int64 {
	return append([]int64(nil), t.keyTimes...)
}

// Analyze probes a file and returns its structure, tracks and declared
// timeline. Structural faults are classified in File.Issues and Track.Issues;
// an error is returned only when the input cannot be opened at all.
func Analyze(path string, cfg Config) (*File, error) {
	return probeFile(path, cfg)
}

// walkTimeline iterates the sample cursor once, validating timestamps and
// data ranges. It records anomalies instead of aborting so partial results
// remain reportable.
func (c *container) walkTimeline(t *Track, st *sampleTable) {
	if t.Timescale == 0 {
		t.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityFatal, "timeline", t.ID, t.DataEnd,
			"zero timescale prevents conversion"))
		return
	}
	if len(st.stts) == 0 {
		return
	}
	cursor := t.Cursor()
	var prevDTS int64 = -1
	var prevCTS int64
	var firstSample = true
	var overflow bool
	var dtsTotal uint64
	for {
		row, ok := cursor.Next()
		if !ok {
			break
		}
		t.Walked++
		if t.FirstSample < 0 {
			t.FirstSample = row.Offset
		}
		end := row.Offset + int64(row.Size)
		if end > t.DataEnd {
			t.DataEnd = end
		}
		if row.OutOfOrder {
			t.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityPartial, "timeline", t.ID, row.Offset,
				fmt.Sprintf("sample %d violates byte order", row.Index+1)))
		}
		if !row.InBounds {
			if t.UnreadableOffset < 0 || row.Offset < t.UnreadableOffset {
				t.UnreadableOffset = row.Offset
			}
			t.TruncatedSamples++
			t.addIssue(fault.ForTrack(fault.CodeTruncated, fault.SeverityPartial, "timeline", t.ID, row.Offset,
				fmt.Sprintf("sample %d [%d,%d) escapes file of %d bytes", row.Index+1, row.Offset, end, t.size)))
		}
		if !firstSample {
			if dtsTotal > uint64(math.MaxInt64) {
				if !overflow {
					t.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityFatal, "timeline", t.ID, row.Offset,
						"decode timestamp exceeds 63 bits"))
					overflow = true
				}
			} else {
				if row.DTSMS < prevDTS {
					t.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityPartial, "timeline", t.ID, row.Offset,
						fmt.Sprintf("timestamp regression at sample %d: %d ms after %d ms", row.Index+1, row.DTSMS, prevDTS)))
				}
				if row.CTSMS < prevCTS {
					t.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityPartial, "composition", t.ID, row.Offset,
						fmt.Sprintf("composition time regression at sample %d", row.Index+1)))
				}
			}
		}
		prevDTS = row.DTSMS
		prevCTS = row.CTSMS
		firstSample = false
		dtsTotal = row.DTS

		if row.Keyframe {
			t.Keyframes++
			c.noteKeyframe(t, row.DTSMS)
		}
	}
	if t.Walked < t.Declared {
		t.addIssue(fault.ForTrack(fault.CodeIndexIncomplete, fault.SeverityPartial, "timeline", t.ID, t.DataEnd,
			fmt.Sprintf("walked %d of %d declared samples", t.Walked, t.Declared)))
	}
	if t.DataEnd < t.FirstSample {
		t.DataEnd = t.FirstSample
	}
}

// noteKeyframe records keyframe spacing statistics within the keyframe cap.
func (c *container) noteKeyframe(t *Track, dtsMS int64) {
	if n := len(t.keyTimes); n > 0 {
		gap := dtsMS - t.keyTimes[n-1]
		if gap >= 0 {
			if gap > t.maxGap {
				t.maxGap = gap
			}
			if t.minGap == 0 || gap < t.minGap {
				t.minGap = gap
			}
			t.gapSum += gap
			t.gapCount++
		}
	}
	if len(t.keyTimes) < t.cfg.MaxKeyframes {
		t.keyTimes = append(t.keyTimes, dtsMS)
	}
	t.MaxKeyframeGapMS = t.maxGap
	t.MinKeyframeGapMS = t.minGap
	if t.gapCount > 0 {
		t.MeanKeyframeGapMS = t.gapSum / t.gapCount
	}
}

// parseEditList derives the track's presentation shift from its edit list.
// An empty edit (media_time -1) delays the track; a normal edit with a
// positive media_time trims the beginning. The shift is expressed in the
// shared millisecond timeline.
func (c *container) parseEditList(t *Track, trakKids []childBox) {
	edts, ok := c.findChild(trakKids, "edts")
	if !ok {
		return
	}
	ekids := c.children(edts.Payload, edts.payloadStart(), "edts")
	elst, ok := c.findChild(ekids, "elst")
	if !ok {
		return
	}
	var box mp4.Elst
	if err := c.decode(elst, &box); err != nil {
		t.addIssue(fault.ForTrack(fault.CodeStructure, fault.SeverityPartial, "elst", t.ID, elst.Offset, err.Error()))
		return
	}
	var emptyMS int64
	for i := 0; i < len(box.Entries); i++ {
		mediaTime := box.GetMediaTime(i)
		if mediaTime == -1 {
			emptyMS = clock.AddMS(emptyMS, clock.TicksToMS(box.GetSegmentDuration(i), t.cfg.movieTimescale(t)))
			continue
		}
		shift := emptyMS
		if mediaTime > 0 {
			shift = clock.AddMS(shift, -clock.TicksToMS(uint64(mediaTime), t.Timescale))
		} else if mediaTime < 0 {
			shift = clock.AddMS(shift, clock.TicksToMS(uint64(-mediaTime), t.Timescale))
		}
		t.StartMS = shift
		return
	}
	t.StartMS = emptyMS
}

// movieTimescale returns the movie header timescale when it is known, else
// the track timescale. Segment durations in an elst are expressed in the
// movie timescale.
func (cfg Config) movieTimescale(t *Track) uint32 {
	if t.movieTimescale != 0 {
		return t.movieTimescale
	}
	return t.Timescale
}
