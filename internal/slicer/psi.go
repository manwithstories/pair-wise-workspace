package slicer

import (
	"fmt"

	"tsprobe/internal/psi"
)

func (c *clipWriter) writePSI(snapshot psi.Snapshot, number int) error {
	programs := make([]psi.PATProgram, 0, len(snapshot.Programs))
	for _, program := range snapshot.Programs {
		programs = append(programs, psi.PATProgram{Program: program.Program, PID: program.PMTPID})
	}
	patVersion := (snapshot.PATVersion + uint8(number) + 1) & 0x1f
	section, err := psi.BuildPAT(snapshot.TransportStreamID, patVersion, programs)
	if err != nil {
		return err
	}
	if err := c.writeSection(0, section); err != nil {
		return err
	}
	c.info.PATVersion = patVersion

	for _, program := range snapshot.Programs {
		if !program.HasPMT {
			continue
		}
		pmtVersion := (program.PMTVersion + uint8(number) + 1) & 0x1f
		pmt, err := psi.BuildPMT(
			program.Program, program.PMT.PCRPID, pmtVersion,
			program.PMT.Streams, program.PMT.Descriptors,
		)
		if err != nil {
			return err
		}
		if err := c.writeSection(program.PMTPID, pmt); err != nil {
			return err
		}
		c.info.PMTVersions = append(c.info.PMTVersions, PMTEntry{
			Program: program.Program, Version: pmtVersion,
		})
	}
	if len(c.info.PMTVersions) == 0 {
		return fmt.Errorf("slicer: no PMT in snapshot")
	}
	return nil
}

func (c *clipWriter) writeSection(pid uint16, section []byte) error {
	var cc uint8
	for _, packet := range psi.PacketizeSection(pid, cc, section) {
		if _, err := c.file.Write(packet); err != nil {
			return err
		}
		c.info.PacketCount++
		c.info.ByteLength += 188
		c.info.psiPackets++
		cc = (cc + 1) & 0x0f
	}
	return nil
}
