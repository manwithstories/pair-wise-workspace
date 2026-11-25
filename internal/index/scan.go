// Package index turns a transport stream into an exploitation index: program
// layout, keyframe positions on one 90 kHz timeline, damaged ranges, and the
// per-track descriptions the deliverer needs. Scanning is deterministic: the
// same bytes yield the same index, byte for byte, so the cache can key on a
// SHA-256 of the input.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"cinepipe/internal/clock"
	"cinepipe/internal/fault"
	"cinepipe/internal/ts"
)

// RangeKind distinguishes a gap caused by dropped transport packets from one
// caused by a structural break (truncation, resync).
type RangeKind string

const (
	RangeContinuity RangeKind = "continuity"
	RangeTruncated  RangeKind = "truncated"
	RangeSync       RangeKind = "sync"
	RangePSI        RangeKind = "psi"
)

// Source identifies the bytes that were scanned. SHA-256 invalidates the cache:
// any change to the input, including a new seed, produces a different hash and
// forces a rebuild.
type Source struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Keyframe is one random-access point on the timeline.
type Keyframe struct {
	Index      int64 `json:"index"`
	ByteOffset int64 `json:"byte_offset"`
	Ticks      int64 `json:"ticks"`
	Keyframe   bool  `json:"keyframe"`
	// Recovered is true when this frame is the first usable one after a damaged
	// range. Its timestamp defines where playable content resumes.
	Recovered bool `json:"recovered"`
}

// DamagedRange is a contiguous span of the timeline and byte stream that cannot
// be trusted. StartTicks and EndTicks place it on the common 90 kHz axis so the
// deliverer can mark the manifest gap without re-parsing.
type DamagedRange struct {
	Kind       RangeKind `json:"kind"`
	StartByte  int64     `json:"start_byte"`
	EndByte    int64     `json:"end_byte"`
	StartTicks int64     `json:"start_ticks"`
	EndTicks   int64     `json:"end_ticks"`
	PID        uint16    `json:"pid,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// RecoveredAt is the byte offset of the keyframe that ends the range, or -1
	// when the range runs to end of input.
	RecoveredAt int64 `json:"recovered_at"`
}

// TrackKind separates the two demux paths.
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
)

// Track is a stream's description plus its per-segment parameter snapshots.
type Track struct {
	Kind        TrackKind    `json:"kind"`
	PID         uint16       `json:"pid"`
	StreamType  uint8        `json:"stream_type"`
	Codec       string       `json:"codec"`
	SampleRate  int          `json:"sample_rate,omitempty"`
	Channels    int          `json:"channels,omitempty"`
	Layout      string       `json:"layout,omitempty"`
	Language    string       `json:"language,omitempty"`
	Color       *ColorInfo   `json:"color,omitempty"`
	Keyframes   []Keyframe   `json:"keyframes,omitempty"`
	Descriptions []TrackDescription `json:"descriptions,omitempty"`
}

// ColorInfo carries the colour primaries, transfer function, and matrix
// coefficients that must match before footage from different cameras is cut
// together.
type ColorInfo struct {
	Primaries  uint16 `json:"primaries"`
	Transfer   uint16 `json:"transfer"`
	Matrix     uint16 `json:"matrix"`
	FullRange  bool   `json:"full_range"`
	String     string `json:"string"`
}

// TrackDescription preserves a track's parameters for one segment. When camera
// descriptions differ, the segments are kept under separate description keys
// rather than normalised onto a common one.
type TrackDescription struct {
	Key        string `json:"key"`
	SampleRate int    `json:"sample_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	Layout     string `json:"layout,omitempty"`
	FirstTicks int64  `json:"first_ticks"`
	LastTicks  int64  `json:"last_ticks"`
}

// Index is the complete scan result.
type Index struct {
	Source   Source         `json:"source"`
	Program  ProgramInfo    `json:"program"`
	Tracks   []Track        `json:"tracks"`
	Ranges   []DamagedRange `json:"ranges"`
	Errors   []fault.Error  `json:"errors"`
	Stats    Stats          `json:"stats"`
	// OriginPCR is the first PCR on the absolute timeline; all timestamps in this
	// index are expressed relative to it, so the deliverer and the scanner share
	// one axis.
	OriginPCR int64 `json:"origin_pcr"`
	FirstError int64 `json:"first_error"`
}

// ProgramInfo is the demux layout discovered from the PAT/PMT.
type ProgramInfo struct {
	Program  uint16 `json:"program"`
	PMTPID   uint16 `json:"pmt_pid"`
	PCRPID   uint16 `json:"pcr_pid"`
	VideoPID uint16 `json:"video_pid,omitempty"`
	AudioPID uint16 `json:"audio_pid,omitempty"`
	Version  uint8  `json:"version"`
	// PMT keeps the parsed table for stream-type lookups after scanning.
	PMT *ts.PMT `json:"-"`
}

// Stats are counts a report can show without iterating the index.
type Stats struct {
	Packets     int64 `json:"packets"`
	PayloadPackets int64 `json:"payload_packets"`
	VideoFrames int64 `json:"video_frames"`
	Keyframes   int64 `json:"keyframes"`
	DamagedFrames int64 `json:"damaged_frames"`
	PSIPackets  int64 `json:"psi_packets"`
}

// Hash returns the SHA-256 of the input in lower-case hex.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Scanner accumulates state while walking transport packets. It is meant to be
// driven one packet at a time by Run, but tests construct pieces directly.
type Scanner struct {
	input []byte

	pat  *ts.PAT
	pmt  *ts.PMT
	prog ProgramInfo

	psi   *ts.SectionAssembler
	cc    *ts.ContinuityTracker
	pcr   *ts.PCRTracker
	video *ts.VideoAssembler
	audio *ts.AudioAssembly

	videoPID uint16
	audioPID uint16
	pcrPID   uint16

	units     []ts.AccessUnit
	errors    fault.List
	ranges    []DamagedRange
	pending   *openRange

	// Timestamps: PTS unwrapping and the first absolute timestamp establish the
	// index timeline origin.
	tsSet     *ts.TimestampSet
	originAbs int64
	firstPCR  int64
	originPTS int64

	lastTicks  int64
	lastOffset int64

	color *ColorInfo

	psiPackets int64
	frames     int64
	damaged    int64
	keyframes  int64
}

// openRange is a damaged range still accruing packets, closed when either the
// byte offset moves past it or a recovery keyframe is found.
type openRange struct {
	kind        RangeKind
	startByte   int64
	endByte     int64
	startTicks  int64
	endTicks    int64
	pid         uint16
	detail      string
	detailCount int
}

// NewScanner returns a scanner for the given input bytes.
func NewScanner(input []byte) *Scanner {
	return &Scanner{
		input:     input,
		psi:       ts.NewSectionAssembler(),
		cc:        ts.NewContinuityTracker(),
		pcr:       ts.NewPCRTracker(),
		tsSet:     ts.NewTimestampSet(),
		originAbs: clock.NoTime,
		firstPCR:  clock.NoTime,
		lastTicks: clock.NoTime,
	}
}

// Run scans the input and returns the completed index.
func Run(input []byte) *Index {
	s := NewScanner(input)
	s.walk()
	return s.finish()
}

// walk iterates transport packets, feeding every parser. Short trailing chunks
// are reported as truncated packets and still scanned so the index can locate
// the loss.
func (s *Scanner) walk() {
	pos := 0
	var idx int64
	for pos < len(s.input) {
		end := pos + ts.PacketSize
		if end > len(s.input) {
			end = len(s.input)
		}
		pkt, errs := ts.ParsePacket(s.input[pos:end], int64(pos), idx)
		for _, e := range errs {
			s.record(e)
		}
		s.observe(pkt)
		pos = end
		idx++
		s.lastOffset = int64(pos)
	}
	s.flush()
}

// observe feeds one parsed packet through continuity, PSI, PCR, and elementary
// stream handling in that order.
func (s *Scanner) observe(p *ts.Packet) {
	for _, e := range s.cc.Observe(p) {
		s.record(e)
	}
	if p.PID == ts.NullPID {
		return
	}
	if p.PID == 0 || (s.prog.PMTPID != 0 && p.PID == s.prog.PMTPID) || s.isPSI(p.PID) {
		s.psiPackets++
		secs, errs := s.psi.Assemble(p)
		for _, e := range errs {
			s.record(e)
		}
		for _, sec := range secs {
			s.handleSection(sec)
		}
	}
	if p.CarriesPCR() {
		if base, ok := ts.ParsePCR(p.Adaptation); ok {
			disc := p.Adaptation.Discontinuity
			abs, _, errs := s.pcr.Observe(p, base, disc)
			for _, e := range errs {
				s.record(e)
			}
			if s.firstPCR == clock.NoTime {
				s.firstPCR = abs
			}
		}
	}
	// Video and audio payloads.
	if p.HasPayload && p.PUSI {
		s.handlePES(p)
	}
}

// advance attempts to establish the timeline origin from the earliest known
// absolute timestamp. Relative ticks are meaningless until this succeeds.
func (s *Scanner) advance(abs int64) {
	if abs == clock.NoTime {
		return
	}
	if s.originAbs == clock.NoTime {
		s.originAbs = abs
	}
	if rel := abs - s.originAbs; rel > s.lastTicks || s.lastTicks == clock.NoTime {
		s.lastTicks = rel
	}
}

// isPSI reports whether a PID carries a table cinepipe still needs. Once the
// PMT is parsed, only the PMT PID and PAT PID carry PSI.
func (s *Scanner) isPSI(pid uint16) bool {
	return pid == 0 || pid == s.prog.PMTPID
}

// handleSection interprets a completed PSI section and updates the program map.
func (s *Scanner) handleSection(sec *ts.Section) {
	switch sec.TableID {
	case ts.TIDPat:
		pat, err := ts.ParsePAT(sec)
		if err != nil {
			s.record(fault.Error{Kind: fault.KindPSI, Offset: sec.Offset, Detail: err.Error()})
			return
		}
		s.pat = pat
		for _, prog := range pat.Programs {
			if prog.PMTPID != 0 {
				s.prog.Program = prog.Program
				s.prog.PMTPID = prog.PMTPID
				break
			}
		}
	case ts.TIDPmt:
		if s.prog.PMTPID != 0 && sec.Ext != s.prog.Program {
			return // a different program's PMT
		}
		pmt, err := ts.ParsePMT(sec)
		if err != nil {
			s.record(fault.Error{Kind: fault.KindPSI, Offset: sec.Offset, Detail: err.Error()})
			return
		}
		s.pmt = pmt
		s.prog.Version = pmt.Version
		s.prog.PCRPID = pmt.PCRPID
		s.prog.PMT = pmt
		s.adoptPMT(pmt)
	}
}

// adoptPMT wires the elementary PIDs into the assemblers after a PMT parse.
func (s *Scanner) adoptPMT(pmt *ts.PMT) {
	if s.pcrPID == 0 {
		s.pcrPID = pmt.PCRPID
	}
	for _, st := range pmt.Streams {
		if st.IsVideo() && s.videoPID == 0 {
			s.videoPID = st.PID
			s.prog.VideoPID = st.PID
			if s.video == nil {
				s.video = ts.NewVideoAssembler(st.StreamType)
			}
		}
		if st.IsAudio() && s.audioPID == 0 {
			s.audioPID = st.PID
			s.prog.AudioPID = st.PID
			if s.audio == nil {
				s.audio = ts.NewAudioAssembly(st.PID, st.StreamType, st.HasLanguage)
			}
		}
	}
}

// handlePES reads a PES header from a PUSI payload and dispatches the
// elementary data to the video or audio assembler. It ignores packets until the
// PMT has assigned the elementary PIDs, so PSI on PID 0 is never mistaken for a
// PES stream.
func (s *Scanner) handlePES(p *ts.Packet) {
	isVideo := s.videoPID != 0 && p.PID == s.videoPID
	isAudio := s.audioPID != 0 && p.PID == s.audioPID
	if !isVideo && !isAudio {
		return
	}
	hdr, ok, reason := ts.ParsePESHeader(p.Payload)
	if !ok {
		s.record(p.Errorf(fault.KindTruncated, "pes header: "+reason))
	}
	var absPTS int64 = clock.NoTime
	if hdr.HasPTS {
		abs, _ := s.tsSet.Observe(hdr)
		if abs != clock.NoTime {
			absPTS = abs
			if s.originPTS == 0 && s.originAbs == clock.NoTime {
				s.originPTS = abs
			}
			s.advance(abs)
		}
	}
	payload := p.Payload
	if hdr.DataStart > 0 && hdr.DataStart <= len(payload) {
		payload = payload[hdr.DataStart:]
	}
	if p.PID == s.videoPID && s.video != nil {
		s.video.PushPES(payload, p.Offset, absPTS)
	}
	if p.PID == s.audioPID && s.audio != nil {
		_, _, errs := s.audio.Describe(payload, p.Offset)
		for _, e := range errs {
			s.record(e)
		}
	}
}

// record files a diagnostic and folds it into damaged-range tracking. A
// diagnostic close to an open range of the same kind extends that range
// instead of opening a new one, so a burst of continuity jumps reads as a single
// gap.
func (s *Scanner) record(e fault.Error) {
	s.errors.Add(e)
	kind := rangeKindFor(e.Kind)
	if kind == "" {
		return
	}
	start := e.Offset
	end := e.Offset + ts.PacketSize
	tick := s.tickFor(e)
	if s.pending != nil {
		gap := start - s.pending.endByte
		if kind == s.pending.kind && gap <= ts.PacketSize*4 {
			s.pending.endByte = end
			s.pending.detailCount++
			if tick != clock.NoTime && (s.pending.endTicks == clock.NoTime || tick > s.pending.endTicks) {
				s.pending.endTicks = tick
			}
			return
		}
		s.closePending()
	}
	s.pending = &openRange{
		kind: kind, startByte: start, endByte: end,
		startTicks: tick, endTicks: tick, pid: e.PID, detail: e.Detail, detailCount: 1,
	}
	s.damaged++
}

// tickFor approximates the timeline position of an error from the most recent
// known timestamp, so a damaged range can be placed on the common axis even
// when the error packet carries no PTS itself.
func (s *Scanner) tickFor(e fault.Error) int64 {
	if e.Ticks != 0 {
		return e.Ticks
	}
	return s.lastTicks
}

// rangeKindFor maps a fault kind onto a damaged-range kind, or "" for
// informational diagnostics that must not carve out a range.
func rangeKindFor(k fault.Kind) RangeKind {
	switch k {
	case fault.KindContinuity:
		return RangeContinuity
	case fault.KindTruncated, fault.KindAdaptation:
		return RangeTruncated
	case fault.KindSync:
		return RangeSync
	case fault.KindCRC, fault.KindPSI:
		return RangePSI
	}
	return ""
}

// closePending seals the open damaged range, recording it.
func (s *Scanner) closePending() {
	if s.pending == nil {
		return
	}
	r := DamagedRange{
		Kind:       s.pending.kind,
		StartByte:  s.pending.startByte,
		EndByte:    s.pending.endByte,
		StartTicks: s.pending.startTicks,
		EndTicks:   s.pending.endTicks,
		PID:        s.pending.pid,
		Detail:     s.pending.detail,
		RecoveredAt: -1,
	}
	s.ranges = append(s.ranges, r)
	s.pending = nil
}

// flush finalizes the video assembler, derives durations, and closes any open
// damaged range at end of input.
func (s *Scanner) flush() {
	if s.video != nil {
		s.video.Flush()
		s.units = s.video.Units()
	}
	s.closePending()
}

// finish assembles the index, resolving recovery keyframes and timeline
// positions.
func (s *Scanner) finish() *Index {
	s.originAbs = s.alignOrigin()
	idx := &Index{
		Source:    Source{SHA256: Hash(s.input), Bytes: int64(len(s.input))},
		Program:   s.prog,
		OriginPCR: s.firstPCR,
		Errors:    s.errors.Sorted(),
		Ranges:    s.ranges,
	}
	if s.firstPCR == clock.NoTime {
		s.firstPCR = 0
		idx.OriginPCR = 0
	}
	if first := s.errors.First(); first >= 0 {
		idx.FirstError = first
	} else {
		idx.FirstError = -1
	}

	// Place keyframes on the shared timeline and identify recovery points.
	idx.Tracks = s.buildTracks()
	idx.Ranges = s.resolveRecovery(idx.Ranges, idx.Tracks)
	idx.Stats = Stats{
		Packets:        idx.Source.Bytes / ts.PacketSize,
		PayloadPackets: s.cc.Packets(s.videoPID) + s.cc.Packets(s.audioPID),
		VideoFrames:    int64(len(s.units)),
		Keyframes:      s.keyframes,
		DamagedFrames:  s.damaged,
		PSIPackets:     s.psiPackets,
	}
	return idx
}

// alignOrigin picks the timeline origin: the first PCR if present, else the
// first video PTS, else zero.
func (s *Scanner) alignOrigin() int64 {
	if s.firstPCR != clock.NoTime {
		return s.firstPCR
	}
	if s.originPTS != 0 {
		return s.originPTS
	}
	return 0
}

// buildTracks turns the assembled elementary streams into track records with
// keyframes on the common axis.
func (s *Scanner) buildTracks() []Track {
	var tracks []Track
	if s.video != nil {
		t := Track{Kind: TrackVideo, PID: s.videoPID, StreamType: streamTypeOf(s.pmt, s.videoPID),
			Codec: s.videoCodec(), Color: s.color}
		for _, u := range s.units {
			kf := Keyframe{
				Index:      u.Index,
				ByteOffset: u.ByteOffset,
				Ticks:      s.relTicks(u.Ticks),
				Keyframe:   u.Keyframe,
			}
			if u.Keyframe {
				s.keyframes++
			}
			t.Keyframes = append(t.Keyframes, kf)
		}
		tracks = append(tracks, t)
	}
	if s.audio != nil && s.audio.Locked() {
		d := s.audio.Description()
		t := Track{
			Kind: TrackAudio, PID: s.audioPID, StreamType: d.StreamType,
			Codec: d.Codec, SampleRate: d.SampleRate, Channels: d.Channels,
			Layout: d.Layout, Language: d.Language,
		}
		desc := TrackDescription{
			Key:        trackKey(d.Codec, d.SampleRate, d.Channels),
			SampleRate: d.SampleRate, Channels: d.Channels, Layout: d.Layout,
			FirstTicks: 0, LastTicks: s.relTicks(s.lastTicks),
		}
		t.Descriptions = append(t.Descriptions, desc)
		tracks = append(tracks, t)
	}
	return tracks
}

// relTicks expresses an absolute tick on the index timeline.
func (s *Scanner) relTicks(abs int64) int64 {
	if abs == clock.NoTime {
		return clock.NoTime
	}
	origin := s.alignOrigin()
	return abs - origin
}

// resolveRecovery attaches the next keyframe after each damaged range and
// trims the range so it ends where usable content resumes. Frames inside a
// damaged span are never offered as usable keyframes.
func (s *Scanner) resolveRecovery(ranges []DamagedRange, tracks []Track) []DamagedRange {
	if len(tracks) == 0 || tracks[0].Kind != TrackVideo {
		return ranges
	}
	keys := tracks[0].Keyframes
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Ticks != keys[j].Ticks {
			return keys[i].Ticks < keys[j].Ticks
		}
		return keys[i].Index < keys[j].Index
	})
	for i := range ranges {
		r := &ranges[i]
		for _, k := range keys {
			if !k.Keyframe {
				continue
			}
			if k.ByteOffset > r.EndByte && k.Ticks != clock.NoTime {
				r.RecoveredAt = k.ByteOffset
				if k.Ticks != clock.NoTime {
					r.EndTicks = k.Ticks
				}
				break
			}
		}
	}
	return ranges
}

// streamTypeOf looks up a PID's declared stream type.
func streamTypeOf(pmt *ts.PMT, pid uint16) uint8 {
	if pmt == nil {
		return 0
	}
	for _, st := range pmt.Streams {
		if st.PID == pid {
			return st.StreamType
		}
	}
	return 0
}

// videoCodec names a stream type.
func (s *Scanner) videoCodec() string {
	switch streamTypeOf(s.pmt, s.videoPID) {
	case ts.StreamTypeH264:
		return "h264"
	case ts.StreamTypeHEVC:
		return "hevc"
	}
	return "unknown"
}

// trackKey builds the stable description key used to keep incompatible tracks
// apart.
func trackKey(codec string, rate, channels int) string {
	return fmt.Sprintf("%s/%d/%d", codec, rate, channels)
}
