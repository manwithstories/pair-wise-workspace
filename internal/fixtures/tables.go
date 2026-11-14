package fixtures

import "tsaudit/internal/psi"

func (b *builder) emitTables(ms float64) {
	patVersion := patVersionAt(ms, b.cfg)
	programs := []psi.PATBuildProgram{{ProgramNumber: ghostProgram, PID: ghostPMTPID}}
	for p := 1; p <= b.cfg.Programs; p++ {
		programs = append(programs, psi.PATBuildProgram{ProgramNumber: uint16(p), PID: pmtPID(p, patVersion)})
	}
	b.emitSection(patPID, psi.BuildPAT(1, uint8(patVersion), true, programs))
	for p := 1; p <= b.cfg.Programs; p++ {
		b.emitPMT(uint16(p), pmtPID(p, patVersion), ms)
	}
}

func (b *builder) emitPMT(program, pmtPID uint16, ms float64) {
	version := uint8(pmtVersionAt(ms, b.cfg))
	var streams []psi.PMTBuildStream
	var info []byte
	if program == 1 {
		info = append([]byte{0x7f, 160}, make([]byte, 160)...)
		streams = []psi.PMTBuildStream{
			{StreamType: 0x1b, PID: p1VideoPID, ESInfo: psi.RegistrationDescriptor("HDMV")},
			{StreamType: 0x0f, PID: p1AudioPID, ESInfo: psi.LanguageDescriptor("eng", 0)},
		}
	} else {
		audioPID := uint16(p2AudioPID)
		if pmtVersionAt(ms, b.cfg) >= 2 {
			audioPID = p2AudioRemap
		}
		streams = []psi.PMTBuildStream{
			{StreamType: 0x1b, PID: p2VideoPID, ESInfo: psi.RegistrationDescriptor("HDMV")},
			{StreamType: 0x0f, PID: audioPID, ESInfo: psi.LanguageDescriptor("zho", 0)},
			{StreamType: 0x06, PID: zeroPID, ESInfo: psi.LanguageDescriptor("spa", 0)},
		}
	}
	section := psi.BuildPMT(program, videoPID(int(program)), version, true, info, streams)
	b.emitSection(pmtPID, section)
}

func (b *builder) emitSection(pid uint16, section []byte) {
	b.sectionSeq++
	if b.corrupt < b.cfg.CRCErrors && b.sectionSeq >= 10 && (b.sectionSeq-10)%9 == 0 {
		section = append([]byte(nil), section...)
		section[len(section)-1] ^= 0xff
		b.corrupt++
	}
	const firstMax = 183
	pos := 0
	first := true
	for pos < len(section) {
		n := 184
		if first {
			n = firstMax
		}
		end := pos + n
		if end > len(section) {
			end = len(section)
		}
		chunk := section[pos:end]
		var payload []byte
		if first {
			payload = make([]byte, 1, 1+len(chunk))
			payload = append(payload, chunk...)
		} else {
			payload = append([]byte(nil), chunk...)
		}
		b.specs = append(b.specs, packetSpec{PID: pid, PUSI: first, CC: b.nextCC(pid), Payload: payload})
		first = false
		pos = end
	}
}

func patVersionAt(ms float64, cfg Config) int {
	return versionAt(ms, cfg.DurationMS, cfg.PATVersions)
}

func pmtVersionAt(ms float64, cfg Config) int {
	return versionAt(ms, cfg.DurationMS, cfg.PMTVersions)
}

func versionAt(ms float64, durationMS, versions int) int {
	if versions <= 1 {
		return 0
	}
	interval := 2000.0
	if float64((versions-1))*interval > float64(durationMS-500) {
		interval = float64(durationMS-500) / float64(versions-1)
	}
	v := int(ms / interval)
	if v >= versions {
		v = versions - 1
	}
	return v
}

func pmtPID(program int, patVersion int) uint16 {
	if program == 1 {
		return p1PMTPID
	}
	if program == 2 && patVersion >= 2 {
		return p2PMTRemap
	}
	return uint16(p2PMTPID + (program-2)*0x10)
}

func videoPID(program int) uint16 {
	if program == 1 {
		return p1VideoPID
	}
	return uint16(p2VideoPID + (program-2)*0x10)
}

func activeDataPIDs(programs, pmtVersion int) []uint16 {
	pids := []uint16{p1VideoPID, p1AudioPID, strayPID}
	if programs >= 2 {
		audio := uint16(p2AudioPID)
		if pmtVersion >= 2 {
			audio = p2AudioRemap
		}
		pids = append(pids, p2VideoPID, audio)
	}
	return pids
}
