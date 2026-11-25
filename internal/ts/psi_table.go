package ts

import (
	"streamcut/internal/media"
)

// PSITracker applies the version/CRC gate: only a complete, CRC-valid PSI
// section can commit a new ProgramState. A corrupt section is rejected, the
// previous good version stays in effect and an ErrCRCReject error is returned
// for metrics.
type PSITracker struct {
	patVersion int // -1 until first valid PAT
	pmtVersion int
	pmtPID     uint16
	current    *ProgramState
	Rejects    int // psi_crc_rejects counter
}

// NewPSITracker returns a tracker with no committed program.
func NewPSITracker() *PSITracker {
	return &PSITracker{patVersion: -1, pmtVersion: -1}
}

// Current returns the last committed program state, or nil before the first
// valid PAT+PMT pair.
func (t *PSITracker) Current() *ProgramState { return t.current }

// PMTPID reports the PID learned from the most recent valid PAT (0 until one
// arrives).
func (t *PSITracker) PMTPID() uint16 { return t.pmtPID }

// Observe handles one validated-or-not section. Sections on unknown PIDs are
// ignored. A CRC failure increments Rejects and returns an ErrCRCReject
// classified error without touching the committed state.
func (t *PSITracker) Observe(pid uint16, s Section) (*ProgramState, error) {
	if !s.ValidCRC() {
		t.Rejects++
		return t.current, media.Wrap(media.ECRCReject, "ts.psi",
			"CRC mismatch; retaining previous PSI version", nil)
	}
	switch {
	case pid == PIDPAT && s.TableID == 0x00:
		return t.commitPAT(s)
	case t.pmtPID != 0 && pid == t.pmtPID && s.TableID == 0x02:
		return t.commitPMT(s)
	default:
		return t.current, nil
	}
}

func (t *PSITracker) commitPAT(s Section) (*ProgramState, error) {
	b := s.Body
	// table_id(1) + syntax/length(2) + tsid(2) + reserved/version(1)
	// + section/last(2) = 8 header bytes; 4 CRC bytes trail the entries.
	lo, hi := 8, len(b)-4
	nextPMT := uint16(0)
	for lo+4 <= hi {
		progNum := uint16(b[lo])<<8 | uint16(b[lo+1])
		pid := uint16(b[lo+2]&0x1F)<<8 | uint16(b[lo+3])
		lo += 4
		if progNum != 0 {
			nextPMT = pid
		}
	}
	if nextPMT == 0 {
		return t.current, media.Wrap(media.EUnrecoverable, "ts.pat",
			"no program entry in PAT", nil)
	}
	t.patVersion = int(s.Version)
	t.pmtPID = nextPMT
	if t.current != nil {
		t.current.PMTPID = nextPMT
	}
	return t.current, nil
}

func (t *PSITracker) commitPMT(s Section) (*ProgramState, error) {
	b := s.Body
	if len(b) < 12+4 {
		return t.current, media.Wrap(media.EUnrecoverable, "ts.pmt",
			"PMT section too short", nil)
	}
	// After the 8 common bytes: pcr_pid(2), program_info_length(2), then
	// elementary stream loop, then CRC32.
	pcrPID := uint16(b[8]&0x1F)<<8 | uint16(b[9])
	infoLen := int(b[10]&0x0F)<<8 | int(b[11])
	lo := 12 + infoLen
	hi := len(b) - 4
	st := &ProgramState{
		Version: s.Version,
		PMTPID:  t.pmtPID,
		PCRPID:  pcrPID,
	}
	for lo+5 <= hi {
		stype := StreamType(b[lo])
		epid := uint16(b[lo+1]&0x1F)<<8 | uint16(b[lo+2])
		esLen := int(b[lo+3]&0x0F)<<8 | int(b[lo+4])
		lo += 5
		if lo+esLen > hi {
			return t.current, media.Wrap(media.EUnrecoverable, "ts.pmt",
				"ES info length overruns section", nil)
		}
		st.Streams = append(st.Streams, PMTStream{StreamType: stype, ElementaryPID: epid})
		classifyStream(st, stype, epid)
		lo += esLen
	}
	t.pmtVersion = int(s.Version)
	t.current = st
	return st, nil
}

func classifyStream(st *ProgramState, stype StreamType, pid uint16) {
	switch stype {
	case STH264:
		st.VideoPID = pid
	case STPCMPrivate:
		st.AudioPID = pid
		st.AudioKind = media.KindAudioPCM16LE
	case STAACADTS:
		// Explicitly unsupported: surfaced in metrics, never decoded or
		// classified as silence.
		st.AudioPID = pid
		st.AudioKind = media.KindAudioUnsupported
	}
}
