package ts

func (d *Demux) PushPacket(raw []byte) ([]AccessUnit, error) {
	p, err := ParsePacket(raw)
	if err != nil {
		d.stats.Unrecoverable++
		return nil, err
	}
	d.packetNo++
	d.stats.Packets++
	if p.PID == PIDNull {
		return nil, nil
	}

	var out []AccessUnit
	var firstErr error
	ccBroken := false
	if cerr := d.cont.Check(p); cerr != nil {
		d.stats.CCJumps++
		ccBroken = true
		firstErr = cerr
	}
	if p.PCRSet {
		if tl := d.timelineOf(d.pcrPID); tl != nil && d.pcrPID != PIDNull {
			v, err := tl.PCR(uint64(p.PCR/300), uint16(p.PCR%300))
			if err != nil {
				firstErr = err
			} else {
				d.lastPCR = v
				d.pcrSeen = true
			}
		}
	}

	// PSI PIDs are always parsed, even before the table marks them current.
	if p.PID == PIDPAT || (d.pmtPID != 0 && p.PID == d.pmtPID) || d.isPMTPID(p.PID) {
		if p.HasPayload {
			secs, err := d.assembleSection(p)
			if err != nil {
				d.stats.PSIErrors++
				if firstErr == nil {
					firstErr = err
				}
			}
			switched := false
			for _, s := range secs {
				if s.TableID == 0x00 {
					d.stats.PATs++
				} else if s.TableID == 0x02 {
					d.stats.PMTs++
				}
				changed, err := d.tables.Apply(s)
				if err != nil {
					d.stats.PSIErrors++
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				if changed {
					switched = true
				}
			}
			if switched {
				if err := d.reconfigure(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		return out, firstErr
	}

	kind, ok := d.streams[p.PID]
	if !ok {
		return nil, firstErr
	}
	acc := d.pes[p.PID]
	if acc == nil {
		acc = &pesAccum{pid: p.PID, kind: kind}
		d.pes[p.PID] = acc
	}
	if p.HasPayload && len(p.Payload) > 0 {
		if p.PUSI {
			// A continuity jump noticed on a new PES start belongs to the
			// previous PES whose tail was lost. Close that one as damaged and
			// start the new PES clean, so an IDR is never poisoned by a loss
			// that happened just before it.
			if len(acc.data) > 0 {
				if ccBroken {
					acc.broken = true
					acc.reason = "continuity_error"
				}
				au := d.finish(acc)
				if au != nil {
					out = append(out, *au)
				}
			}
			acc.data = acc.data[:0]
			acc.first = d.packetNo
			acc.broken = false
			acc.reason = ""
			acc.hdr = nil
			ccBroken = false
		} else if len(acc.data) == 0 {
			// Continuation without a start: the PES head was lost.
			return out, firstErr
		}
		if ccBroken {
			acc.broken = true
			acc.reason = "continuity_error"
		}
		acc.last = d.packetNo
		acc.data = append(acc.data, p.Payload...)
	}
	return out, firstErr
}

func (d *Demux) finish(acc *pesAccum) *AccessUnit {
	if len(acc.data) == 0 {
		return nil
	}
	hdr, err := ParsePES(acc.data)
	au := &AccessUnit{
		PID:           acc.pid,
		Kind:          acc.kind,
		PTS:           -1,
		DTS:           -1,
		FirstPacket:   acc.first,
		LastPacket:    acc.last,
		Unrecoverable: acc.broken,
		Reason:        acc.reason,
	}
	if err != nil {
		au.Unrecoverable = true
		au.Reason = "pes_header"
		d.stats.Unrecoverable++
		d.orderCounter++
		return au
	}
	if hdr.HasPTS {
		au.PTS = d.timelineOf(acc.pid).PTS(hdr.PTSRaw)
		au.HasPTS = true
	}
	if hdr.HasDTS {
		au.DTS = d.timelineOf(acc.pid).DTS(hdr.DTSRaw)
		au.HasDTS = true
	}
	start := hdr.DataStart
	end := len(acc.data)
	if hdr.PacketLen > 0 {
		// A bounded PES declares its length after the 6-byte start code.
		if stop := 6 + hdr.PacketLen; stop < end {
			end = stop
		}
	}
	if start > end {
		start = end
	}
	au.Data = append([]byte(nil), acc.data[start:end]...)
	if au.Kind == KindVideo {
		if IsIDR(au.Data) {
			au.Keyframe = true
			d.stats.IDRs++
		}
		d.stats.VideoAUs++
		if au.HasPTS {
			d.lastVideoPTS = au.PTS
			d.videoSeen = true
		}
	} else if au.Kind == KindAudio {
		d.stats.AudioAUs++
		if au.HasPTS {
			d.lastAudioPTS = au.PTS
			d.audioSeen = true
		}
	}
	if au.Unrecoverable {
		d.stats.Unrecoverable++
	}
	d.orderCounter++
	return au
}
