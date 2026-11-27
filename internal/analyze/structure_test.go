package analyze

import (
	"testing"

	"tscheck/internal/model"
	"tscheck/internal/ts"
)

func TestStructureReport(t *testing.T) {
	s := NewStructure()
	s.SetStreams([]ts.StreamInfo{{PID: 0x100, StreamType: 0x1B}})
	s.AddPacket(ts.PacketInfo{PID: 0x100, Payload: make([]byte, 100)})
	s.AddPacket(ts.PacketInfo{PID: 0x100, Payload: make([]byte, 84)})
	s.AddPacket(ts.PacketInfo{PID: 0x101, Payload: make([]byte, 10)})
	s.AddPTS(0x100, model.Timestamp{Ticks: 900000, Epoch: 0, Anchored: true})
	s.AddPTS(0x100, model.Timestamp{Ticks: 1800000, Epoch: 0, Anchored: true})
	s.AddPTS(0x101, model.Timestamp{Ticks: 5, Epoch: -1, Anchored: false})
	rep := s.Report()
	if len(rep) != 2 {
		t.Fatalf("tracks = %d", len(rep))
	}
	v := rep[0]
	if v.PID != 0x100 || v.StreamType != 0x1B || v.Packets != 2 || v.PayloadBytes != 184 {
		t.Fatalf("video track = %+v", v)
	}
	if v.FirstPTS != 900000 || v.LastPTS != 1800000 || !v.SeenPTS {
		t.Fatalf("pts range = %+v", v)
	}
	a := rep[1]
	if !a.UnanchoredPTS || a.SeenPTS {
		t.Fatalf("unanchored pts must not enter range: %+v", a)
	}
}
