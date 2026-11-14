package fixture

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"time"

	"tsprobe/internal/psi"
)

const (
	videoPID    = uint16(0x0100)
	pmtPID      = uint16(0x1000)
	danglingPID = uint16(0x1001)
)

type Options struct {
	Programs int
	Duration time.Duration
	Loss     int
	CCJumps  int
	Versions int
}

type Summary struct {
	Packets      int
	VideoPackets int
	PMTPackets   int
	Lost         int
	CCJumps      int
	PATVersions  int
	PMTVersions  int
}

type eventKind int

const (
	eventPAT eventKind = iota
	eventPMT
	eventVideo
	eventNull
	eventAdaptOnly
	eventTEI
	eventGarbage
)

type streamEvent struct {
	at       int64
	priority int
	kind     eventKind
	index    int
	version  uint8
	pid      uint16
}

type videoPacket struct {
	keyframe bool
	pusi     bool
	pts      int64
	payload  []byte
}

type generator struct {
	w       *bufio.Writer
	opts    Options
	summary Summary
	cc      map[uint16]uint8
	bytes   int64
	events  []streamEvent
	video   []videoPacket
	drops   map[int]struct{}
	jumps   map[int]struct{}
}

// Generate writes a deterministic synthetic transport stream.
func Generate(w io.Writer, opts Options) (Summary, error) {
	if opts.Programs < 1 {
		opts.Programs = 1
	}
	if opts.Duration <= 0 {
		return Summary{}, fmt.Errorf("fixture: duration must be positive")
	}
	if opts.Versions < 1 {
		opts.Versions = 1
	}
	if opts.Versions > 32 {
		opts.Versions = 32
	}
	if opts.Loss < 0 {
		opts.Loss = 0
	}
	if opts.CCJumps < 0 {
		opts.CCJumps = 0
	}
	g := &generator{w: bufio.NewWriterSize(w, 64*1024), opts: opts, cc: make(map[uint16]uint8)}
	if err := g.buildEvents(); err != nil {
		return Summary{}, err
	}
	sort.SliceStable(g.events, func(i, j int) bool {
		if g.events[i].at != g.events[j].at {
			return g.events[i].at < g.events[j].at
		}
		return g.events[i].priority < g.events[j].priority
	})
	if err := g.writeEvents(); err != nil {
		return Summary{}, err
	}
	if err := g.w.Flush(); err != nil {
		return Summary{}, err
	}
	g.summary.PATVersions = minInt(g.opts.Versions, 32)
	g.summary.PMTVersions = pmtVersionCount(g.opts.Versions)
	return g.summary, nil
}

func (g *generator) buildEvents() error {
	duration := int64(g.opts.Duration)
	patCount := g.opts.Versions
	for i := 0; i < patCount; i++ {
		at := duration * int64(i) / int64(maxInt(patCount, 1))
		g.events = append(g.events, streamEvent{at: at, priority: 0, kind: eventPAT, version: uint8(i)})
	}
	if patCount >= 3 {
		g.events = append(g.events, streamEvent{
			at: duration * 96 / 100, priority: 0, kind: eventPAT, version: uint8(patCount - 2),
		})
	}
	pmtCount := int(g.opts.Duration.Seconds() * 2)
	if pmtCount < 1 {
		pmtCount = 1
	}
	for i := 0; i < pmtCount; i++ {
		at := duration * int64(i) / int64(pmtCount)
		version := pmtVersionAt(i, pmtCount, g.opts.Versions)
		g.events = append(g.events, streamEvent{at: at, priority: 1, kind: eventPMT, version: version})
	}
	g.events = append(g.events, streamEvent{at: 0, priority: 2, kind: eventGarbage})

	videoCount := int(float64(g.opts.Duration) / float64(12*time.Second) * 1187)
	if videoCount < 40 {
		videoCount = 40
	}
	packets := buildVideoPackets(videoCount, g.opts.Duration)
	drops := chooseIndices(videoCount, g.opts.Loss, nil)
	jumpAvoid := make(map[int]struct{}, len(drops)*3)
	for idx := range drops {
		jumpAvoid[idx-1] = struct{}{}
		jumpAvoid[idx] = struct{}{}
		jumpAvoid[idx+1] = struct{}{}
	}
	jumps := chooseIndices(videoCount, g.opts.CCJumps, jumpAvoid)
	for i, packet := range packets {
		at := duration * int64(i) / int64(videoCount)
		kind := eventVideo
		if _, dropped := drops[i]; dropped {
			g.summary.Lost++
			continue
		}
		if _, jump := jumps[i]; jump {
			g.summary.CCJumps++
		}
		g.summary.VideoPackets++
		g.events = append(g.events, streamEvent{at: at, priority: 3, kind: kind, index: i})
		_ = packet
	}
	for i := 0; i < maxInt(1, int(g.opts.Duration.Seconds())); i++ {
		at := duration * int64(i) / int64(maxInt(1, int(g.opts.Duration.Seconds())))
		g.events = append(g.events, streamEvent{at: at, priority: 4, kind: eventNull})
	}
	g.events = append(g.events,
		streamEvent{at: duration / 3, priority: 5, kind: eventAdaptOnly},
		streamEvent{at: 2 * duration / 3, priority: 5, kind: eventAdaptOnly},
		streamEvent{at: duration / 4, priority: 5, kind: eventTEI},
		streamEvent{at: 2 * duration / 3, priority: 4, kind: eventGarbage},
	)
	g.video = packets
	g.drops = drops
	g.jumps = jumps
	return nil
}

func buildVideoPackets(count int, duration time.Duration) []videoPacket {
	out := make([]videoPacket, count)
	const pesSize = 10
	totalTicks := int64(duration) * 90000 / int64(time.Second)
	keyframes := map[int]bool{0: true}
	for _, base := range []int{count / 3, 2 * count / 3} {
		idx := (base / pesSize) * pesSize
		if idx > 0 && idx < count-1 {
			keyframes[idx] = true
		}
	}
	for i := range out {
		out[i].pusi = i%pesSize == 0
		out[i].keyframe = keyframes[i]
		if out[i].pusi {
			out[i].pts = totalTicks * int64(i) / int64(count)
			out[i].payload = makePESPayload(out[i].pts, out[i].keyframe)
		} else {
			out[i].payload = make([]byte, 184)
			for j := range out[i].payload {
				out[i].payload[j] = 0x33
			}
		}
	}
	return out
}

func makePESPayload(pts int64, keyframe bool) []byte {
	payload := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5}
	payload = append(payload, encodePTS(pts)...)
	nal := byte(0x41)
	if keyframe {
		nal = 0x65
	}
	payload = append(payload, 0, 0, 1, nal)
	for len(payload) < 150 {
		payload = append(payload, 0x55)
	}
	return payload
}

func encodePTS(pts int64) []byte {
	return []byte{
		0x21 | byte((pts>>29)&0x0e),
		byte(pts >> 22),
		0x01 | byte((pts>>14)&0xfe),
		byte(pts >> 7),
		0x01 | byte((pts<<1)&0xfe),
	}
}

func chooseIndices(total, count int, avoid map[int]struct{}) map[int]struct{} {
	out := make(map[int]struct{})
	if count <= 0 || total <= 0 {
		return out
	}
	candidates := make([]int, 0, total)
	for idx := 1; idx < total; idx++ {
		if idx%10 == 0 {
			continue
		}
		if _, blocked := avoid[idx]; blocked {
			continue
		}
		candidates = append(candidates, idx)
	}
	if count > len(candidates) {
		count = len(candidates)
	}
	for n := 0; n < count; n++ {
		pos := (n + 1) * len(candidates) / (count + 1)
		for pos < len(candidates) {
			if _, used := out[candidates[pos]]; !used {
				break
			}
			pos++
		}
		if pos == len(candidates) {
			for pos = 0; pos < len(candidates); pos++ {
				if _, used := out[candidates[pos]]; !used {
					break
				}
			}
		}
		out[candidates[pos]] = struct{}{}
	}
	return out
}

func pmtVersionCount(versions int) int {
	if versions <= 1 {
		return 1
	}
	return versions - 1
}

func pmtVersionAt(i, total, versions int) uint8 {
	unique := pmtVersionCount(versions)
	if unique == 1 {
		return 0
	}
	if i >= total-total/10 && unique >= 2 {
		return uint8(unique - 2)
	}
	version := i * unique / total
	if version >= unique {
		version = unique - 1
	}
	return uint8(version)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (g *generator) writeEvents() error {
	services := make([]psi.Service, g.opts.Programs)
	for i := range services {
		services[i] = psi.Service{ID: uint16(i + 1), ServiceType: 0x01}
	}
	sdt, err := psi.BuildSDT(1, 1, 0, services)
	if err != nil {
		return err
	}
	if err := g.writeSection(0x11, sdt); err != nil {
		return err
	}

	var videoCC uint8
	nextVideo := 0
	for _, event := range g.events {
		switch event.kind {
		case eventPAT:
			programs := make([]psi.PATProgram, 0, g.opts.Programs)
			programs = append(programs, psi.PATProgram{Program: 1, PID: pmtPID})
			for i := 1; i < g.opts.Programs; i++ {
				programs = append(programs, psi.PATProgram{Program: uint16(i + 1), PID: danglingPID + uint16(i-1)})
			}
			section, err := psi.BuildPAT(1, event.version, programs)
			if err != nil {
				return err
			}
			if err := g.writeSection(0, section); err != nil {
				return err
			}
		case eventPMT:
			section, err := psi.BuildPMT(1, videoPID, event.version, []psi.ESInfo{{
				StreamType: 0x1b, ElementaryPID: videoPID,
			}}, nil)
			if err != nil {
				return err
			}
			if err := g.writeSection(pmtPID, section); err != nil {
				return err
			}
			g.summary.PMTPackets++
		case eventVideo:
			for nextVideo < event.index {
				videoCC = (videoCC + 1) & 0x0f
				nextVideo++
			}
			if _, jump := g.jumps[event.index]; jump {
				videoCC = (videoCC + 9) & 0x0f
			}
			packet := g.video[event.index]
			if err := g.writePacket(buildMediaPacket(videoPID, videoCC, packet)); err != nil {
				return err
			}
			videoCC = (videoCC + 1) & 0x0f
			nextVideo = event.index + 1
		case eventNull:
			if err := g.writePacket(buildNullPacket()); err != nil {
				return err
			}
		case eventAdaptOnly:
			if err := g.writePacket(buildAdaptationOnly(0x0101)); err != nil {
				return err
			}
		case eventTEI:
			payload := make([]byte, 184)
			packet := buildPayloadPacket(0x0102, 0, false, false, payload)
			packet[1] |= 0x80
			if err := g.writePacket(packet); err != nil {
				return err
			}
		case eventGarbage:
			if _, err := g.w.Write([]byte{0xde, 0xad, 0xbe, 0xef, 0xaa, 0x55, 0x99}); err != nil {
				return err
			}
			g.bytes += 7
		}
	}
	return nil
}

func (g *generator) writeSection(pid uint16, section []byte) error {
	cc := g.cc[pid]
	for _, packet := range psi.PacketizeSection(pid, cc, section) {
		if err := g.writePacket(packet); err != nil {
			return err
		}
		cc = (cc + 1) & 0x0f
	}
	g.cc[pid] = cc
	return nil
}

func (g *generator) writePacket(packet []byte) error {
	if len(packet) != 188 {
		return fmt.Errorf("fixture: packet length %d", len(packet))
	}
	if _, err := g.w.Write(packet); err != nil {
		return err
	}
	g.bytes += 188
	g.summary.Packets++
	return nil
}

func buildMediaPacket(pid uint16, cc uint8, packet videoPacket) []byte {
	if packet.keyframe {
		base := uint64(packet.pts)
		pcr := []byte{
			byte(base >> 25), byte(base >> 17), byte(base >> 9), byte(base >> 1),
			byte(base&1)<<7 | 0x7e, 0,
		}
		raw := buildPayloadPacket(pid, cc, packet.pusi, true, packet.payload, pcr)
		return raw
	}
	return buildPayloadPacket(pid, cc, packet.pusi, false, packet.payload)
}

func buildPayloadPacket(pid uint16, cc uint8, pusi, randomAccess bool, payload []byte, pcr ...[]byte) []byte {
	out := make([]byte, 188)
	for i := range out {
		out[i] = 0xff
	}
	out[0] = 0x47
	out[1] = byte(pid >> 8)
	if pusi {
		out[1] |= 0x40
	}
	out[2] = byte(pid)
	payloadAt := 4
	if randomAccess {
		out[3] = 0x30 | (cc & 0x0f)
		out[4] = 7
		out[5] = 0x50
		copy(out[6:12], pcr[0])
		payloadAt = 12
	} else {
		out[3] = 0x10 | (cc & 0x0f)
	}
	copy(out[payloadAt:], payload)
	return out
}

func buildNullPacket() []byte {
	packet := make([]byte, 188)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = 0x47
	packet[1] = 0x1f
	packet[2] = 0xff
	packet[3] = 0x10
	return packet
}

func buildAdaptationOnly(pid uint16) []byte {
	packet := make([]byte, 188)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = 0x47
	packet[1] = byte(pid >> 8)
	packet[2] = byte(pid)
	packet[3] = 0x20
	packet[4] = 183
	packet[5] = 0x80
	return packet
}
