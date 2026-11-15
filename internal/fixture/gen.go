package fixture

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"tsprobe/internal/psi"
)

const (
	packetsPerSecond = 2500
	psiInterval      = 1250
	pcrTicksPerPack  = 36
)

type Options struct {
	Seed         int64
	Programs     int
	Streams      int
	Seconds      int
	Drop         int
	CCJump       int
	PSIRevisions int
	PacketSize   int
}

type Result struct {
	PacketSize     int
	PlannedPackets int64
	WrittenPackets int64
	DroppedPackets int
	CCAhead        int
	CCBackwards    int
	PATRevisions   int
	PMTRevisions   []int
}

type generator struct {
	opts     Options
	writer   io.Writer
	total    int64
	cc       map[uint16]uint8
	cursor   int
	result   Result
	revSlots []int64
	dropAt   map[int64]bool
	jumpAt   map[int64]bool
}

func Generate(w io.Writer, o Options) (Result, error) {
	g, err := newGenerator(w, o)
	if err != nil {
		return Result{}, err
	}
	return g.run()
}

func GenerateFile(path string, o Options) (Result, error) {
	f, err := os.Create(path)
	if err != nil {
		return Result{}, err
	}
	result, runErr := Generate(f, o)
	closeErr := f.Close()
	if runErr != nil {
		return result, runErr
	}
	return result, closeErr
}

func newGenerator(w io.Writer, o Options) (*generator, error) {
	if o.PacketSize == 0 {
		o.PacketSize = 188
	}
	if o.Programs == 0 {
		o.Programs = 2
	}
	if o.Streams == 0 {
		o.Streams = 3
	}
	if o.Seconds == 0 {
		o.Seconds = 20
	}
	if o.PSIRevisions == 0 {
		o.PSIRevisions = 2
	}
	if o.Programs < 1 || o.Programs > 10 || o.Streams < 1 || o.Streams > 8 {
		return nil, fmt.Errorf("fixture: programs and streams are out of range")
	}
	if o.Seconds < 1 || o.Drop < 0 || o.CCJump < 0 || o.PSIRevisions < 1 || o.PSIRevisions > 31 {
		return nil, fmt.Errorf("fixture: invalid generation parameters")
	}
	if o.PacketSize != 188 && o.PacketSize != 192 && o.PacketSize != 204 {
		return nil, fmt.Errorf("fixture: packet size must be 188, 192 or 204")
	}
	total := int64(o.Seconds * packetsPerSecond)
	g := &generator{
		opts: o, writer: w, total: total, cc: make(map[uint16]uint8),
		cursor: int(o.Seed % int64(o.Programs*o.Streams)),
		result: Result{PacketSize: o.PacketSize, PlannedPackets: total,
			PATRevisions: o.PSIRevisions, PMTRevisions: make([]int, o.Programs)},
	}
	for i := range g.result.PMTRevisions {
		g.result.PMTRevisions[i] = o.PSIRevisions
	}
	for revision := 1; revision < o.PSIRevisions; revision++ {
		target := total * int64(revision) / int64(o.PSIRevisions)
		slot := (target + psiInterval - 1) / psiInterval * psiInterval
		if slot >= total {
			slot = total - 1
		}
		g.revSlots = append(g.revSlots, slot)
	}
	isNormal := func(index int64) bool { return g.normalES(index) }
	g.dropAt = selectSites(total, o.Drop, isNormal, nil)
	g.jumpAt = selectSites(total, o.CCJump, isNormal, g.dropAt)
	g.result.DroppedPackets = len(g.dropAt)
	g.result.CCAhead = len(g.dropAt)
	g.result.CCBackwards = len(g.jumpAt)
	return g, nil
}

func selectSites(total int64, count int, normal func(int64) bool, exclude map[int64]bool) map[int64]bool {
	out := make(map[int64]bool)
	if count == 0 {
		return out
	}
	for i := 1; i <= count; i++ {
		pos := total * int64(i) / int64(count+1)
		for pos < total {
			if normal(pos) && !exclude[pos] && !out[pos] {
				out[pos] = true
				break
			}
			pos++
		}
	}
	return out
}

func (g *generator) run() (Result, error) {
	buffered := bufio.NewWriterSize(g.writer, 64*1024)
	g.writer = buffered
	for index := int64(0); index < g.total; index++ {
		if err := g.emitSlot(index); err != nil {
			return g.result, err
		}
	}
	if err := buffered.Flush(); err != nil {
		return g.result, err
	}
	return g.result, nil
}

func (g *generator) emitSlot(index int64) error {
	if index%psiInterval == 0 {
		if err := g.emitPSI(index); err != nil {
			return err
		}
		return nil
	}
	if program := g.randomAccessProgram(index); program > 0 {
		return g.emitESPES(index, program, 0, true)
	}
	if index%5 == 4 {
		payload := make([]byte, 184)
		for i := range payload {
			payload[i] = 0xff
		}
		return g.emitWithCC(0x1fff, false, g.nextCC(0x1fff), payload, false, 0)
	}
	program, stream := g.nextES()
	if g.dropAt[index] {
		g.nextCC(g.esPID(program, stream))
		return nil
	}
	return g.emitESPES(index, program, stream, false)
}

func (g *generator) emitESPES(index int64, program, stream int, randomAccess bool) error {
	revision := g.revisionAt(index)
	pid := g.esPID(program, stream)
	streamType := g.streamType(program, stream, revision)
	payload, err := buildPES(g.opts.Seed, index, pid, streamType, stream)
	if err != nil {
		return err
	}
	base := g.cc[pid]
	cc := base
	next := (base + 1) & 0x0f
	if g.jumpAt[index] {
		cc = (base + 10) & 0x0f
		next = (cc + 1) & 0x0f
	}
	g.cc[pid] = next
	return g.emitWithCC(pid, true, cc, payload, randomAccess, index*pcrTicksPerPack*300)
}

func (g *generator) emitPSI(index int64) error {
	revision := g.revisionAt(index)
	if err := g.emitTables(revision, true, false); err != nil {
		return err
	}
	if preview, ok := g.previewAt(index); ok {
		if err := g.emitTables(preview, false, true); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) emitTables(revision int, currentNext, preview bool) error {
	pat, err := g.patSection(revision, currentNext)
	if err != nil {
		return err
	}
	if err := g.emitPSISection(pat); err != nil {
		return err
	}
	for program := 1; program <= g.opts.Programs; program++ {
		pmtPID := g.pmtPID(program, revision)
		if preview {
			pmtPID = g.pmtPID(program, revision-1)
		}
		section, err := g.pmtSection(program, revision, currentNext)
		if err != nil {
			return err
		}
		if err := g.emitPSISectionOnPID(pmtPID, section); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) emitPSISection(section []byte) error {
	return g.emitPSISectionOnPID(0, section)
}

func (g *generator) emitPSISectionOnPID(pid uint16, section []byte) error {
	if len(section) > 183 {
		return fmt.Errorf("fixture: PSI section is too large for one packet")
	}
	payload := make([]byte, 184)
	payload[0] = 0
	copy(payload[1:], section)
	for i := 1 + len(section); i < len(payload); i++ {
		payload[i] = 0xff
	}
	return g.emitWithCC(pid, true, g.nextCC(pid), payload, false, 0)
}

func (g *generator) emitWithCC(pid uint16, pusi bool, cc uint8, payload []byte, randomAccess bool, pcr int64) error {
	packet, err := buildTSPacket(pid, pusi, cc, payload, randomAccess, pcr)
	if err != nil {
		return err
	}
	out := packet
	switch g.opts.PacketSize {
	case 192:
		out = append(make([]byte, 4), packet...)
	case 204:
		out = append(packet, make([]byte, 16)...)
	}
	if _, err := g.writer.Write(out); err != nil {
		return err
	}
	g.result.WrittenPackets++
	return nil
}

func (g *generator) nextCC(pid uint16) uint8 {
	cc := g.cc[pid]
	g.cc[pid] = (cc + 1) & 0x0f
	return cc
}

func (g *generator) nextES() (int, int) {
	program := g.cursor/g.opts.Streams + 1
	stream := g.cursor % g.opts.Streams
	g.cursor = (g.cursor + 1) % (g.opts.Programs * g.opts.Streams)
	return program, stream
}

func (g *generator) normalES(index int64) bool {
	return index%psiInterval != 0 && g.randomAccessProgram(index) == 0 && index%5 != 4
}

func (g *generator) randomAccessProgram(index int64) int {
	rem := index % packetsPerSecond
	for program := 1; program <= g.opts.Programs; program++ {
		if rem == int64(10+(program-1)*20) {
			return program
		}
	}
	return 0
}

func (g *generator) revisionAt(index int64) int {
	revision := 0
	for i, slot := range g.revSlots {
		if index >= slot {
			revision = i + 1
		}
	}
	return revision
}

func (g *generator) previewAt(index int64) (int, bool) {
	for i, slot := range g.revSlots {
		if index == slot-psiInterval {
			return i + 1, true
		}
	}
	return 0, false
}

func (g *generator) pmtPID(program, revision int) uint16 {
	return uint16(0x1000 + program*0x40 + revision*0x08)
}

func (g *generator) esPID(program, stream int) uint16 {
	return uint16(0x0100 + program*0x100 + (stream+1)*0x10)
}

func (g *generator) streamType(program, stream, revision int) uint8 {
	types := [...]uint8{0x1b, 0x0f, 0x03, 0x04, 0x06, 0x81}
	base := ((program-1)*g.opts.Streams + stream) % len(types)
	streamType := types[base]
	if revision > 0 && stream == (revision-1)%g.opts.Streams {
		streamType = types[(base+revision)%len(types)]
	}
	return streamType
}

func (g *generator) patSection(revision int, currentNext bool) ([]byte, error) {
	sectionLength := 9 + 4*g.opts.Programs
	total := 3 + sectionLength
	if total > 4096 {
		return nil, fmt.Errorf("fixture: PAT section is too long")
	}
	section := make([]byte, total)
	section[0] = 0x00
	section[1] = 0xb0 | byte(sectionLength>>8)
	section[2] = byte(sectionLength)
	section[3], section[4] = 0x00, 0x01
	section[5] = versionByte(revision, currentNext)
	section[6], section[7] = 0x00, 0x00
	pos := 8
	for program := 1; program <= g.opts.Programs; program++ {
		pid := g.pmtPID(program, revision)
		section[pos] = 0x00
		section[pos+1] = byte(program)
		section[pos+2] = 0xe0 | byte(pid>>8)
		section[pos+3] = byte(pid)
		pos += 4
	}
	appendCRC(section)
	return section, nil
}

func (g *generator) pmtSection(program, revision int, currentNext bool) ([]byte, error) {
	sectionLength := 13 + 5*g.opts.Streams
	total := 3 + sectionLength
	if total > 4096 {
		return nil, fmt.Errorf("fixture: PMT section is too long")
	}
	section := make([]byte, total)
	section[0] = 0x02
	section[1] = 0xb0 | byte(sectionLength>>8)
	section[2] = byte(sectionLength)
	section[3] = byte(program >> 8)
	section[4] = byte(program)
	section[5] = versionByte(revision, currentNext)
	section[6], section[7] = 0x00, 0x00
	pcrPID := g.esPID(program, 0)
	section[8] = 0xe0 | byte(pcrPID>>8)
	section[9] = byte(pcrPID)
	section[10], section[11] = 0xf0, 0x00
	pos := 12
	for stream := 0; stream < g.opts.Streams; stream++ {
		pid := g.esPID(program, stream)
		section[pos] = g.streamType(program, stream, revision)
		section[pos+1] = 0xe0 | byte(pid>>8)
		section[pos+2] = byte(pid)
		section[pos+3], section[pos+4] = 0xf0, 0x00
		pos += 5
	}
	appendCRC(section)
	return section, nil
}

func buildPES(seed, index int64, pid uint16, streamType uint8, stream int) ([]byte, error) {
	video := streamType == 0x1b || streamType == 0x02 || streamType == 0x10
	streamID := byte(0xbd)
	switch {
	case video:
		streamID = 0xe0 | byte(stream&0x0f)
	case streamType == 0x03 || streamType == 0x04 || streamType == 0x0f || streamType == 0x81 || streamType == 0x87:
		streamID = 0xc0 | byte(stream&0x1f)
	}
	hasDTS := video
	optionalLength := 5
	if hasDTS {
		optionalLength = 10
	}
	payload := make([]byte, 120)
	for i := range payload {
		payload[i] = byte(uint64(seed) + uint64(index*31) + uint64(pid) + uint64(i*17))
	}
	packetLength := 3 + optionalLength + len(payload)
	out := make([]byte, 0, 6+packetLength)
	out = append(out, 0x00, 0x00, 0x01, streamID, byte(packetLength>>8), byte(packetLength), 0x80)
	if hasDTS {
		out = append(out, 0xc0, byte(optionalLength))
	} else {
		out = append(out, 0x80, byte(optionalLength))
	}
	pts := index * pcrTicksPerPack
	ptsPrefix := byte(0x02)
	if hasDTS {
		ptsPrefix = 0x03
	}
	out = append(out, encodeTimestamp(ptsPrefix, pts)...)
	if hasDTS {
		dts := pts - 3600
		if dts < 0 {
			dts = 0
		}
		out = append(out, encodeTimestamp(0x01, dts)...)
	}
	out = append(out, payload...)
	return out, nil
}

func buildTSPacket(pid uint16, pusi bool, cc uint8, payload []byte, randomAccess bool, pcr int64) ([]byte, error) {
	payloadCapacity := 184
	if randomAccess {
		payloadCapacity = 176
	}
	if len(payload) > payloadCapacity {
		return nil, fmt.Errorf("fixture: payload of %d bytes exceeds capacity %d", len(payload), payloadCapacity)
	}
	packet := make([]byte, 188)
	packet[0] = 0x47
	packet[1] = byte(pid>>8) & 0x1f
	if pusi {
		packet[1] |= 0x40
	}
	packet[2] = byte(pid)
	offset := 4
	if randomAccess {
		packet[3] = 0x30 | cc&0x0f
		packet[4] = 7
		packet[5] = 0x50
		encodePCR(packet[6:12], pcr)
		offset = 12
	} else {
		packet[3] = 0x10 | cc&0x0f
	}
	copy(packet[offset:], payload)
	for i := offset + len(payload); i < len(packet); i++ {
		packet[i] = 0xff
	}
	return packet, nil
}

func versionByte(version int, currentNext bool) byte {
	value := byte(0xc0 | ((version & 0x1f) << 1))
	if currentNext {
		value |= 0x01
	}
	return value
}

func appendCRC(section []byte) {
	crc := psi.CRC32MPEG(section[:len(section)-4])
	section[len(section)-4] = byte(crc >> 24)
	section[len(section)-3] = byte(crc >> 16)
	section[len(section)-2] = byte(crc >> 8)
	section[len(section)-1] = byte(crc)
}

func encodeTimestamp(prefix byte, value int64) []byte {
	if value < 0 {
		value = 0
	}
	return []byte{
		(prefix << 4) | (byte((value>>30)&0x07) << 1) | 1,
		byte(value >> 22),
		(byte((value>>15)&0x7f) << 1) | 1,
		byte(value >> 7),
		(byte(value&0x7f) << 1) | 1,
	}
}

func encodePCR(out []byte, pcr int64) {
	if pcr < 0 {
		pcr = 0
	}
	base := pcr / 300
	extension := pcr % 300
	out[0] = byte(base >> 25)
	out[1] = byte(base >> 17)
	out[2] = byte(base >> 9)
	out[3] = byte(base >> 1)
	out[4] = byte((base&1)<<7) | 0x7e | byte((extension>>8)&1)
	out[5] = byte(extension)
}
