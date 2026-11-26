package fixture

import (
	"bytes"
	"math/rand"

	"mediad/internal/model"
	"mediad/internal/transport"
)

type BandwidthChange struct {
	AfterPackets int   `json:"after_packets"`
	Bps          int64 `json:"bps"`
}

type NetworkPlan struct {
	StreamID      string             `json:"stream_id"`
	PacketSize    int                `json:"packet_size"`
	Packets       []transport.Packet `json:"-"`
	SendOrder     []int              `json:"send_order"`
	DuplicateSeqs []int              `json:"duplicate_seqs"`
	BurstDropped  []int              `json:"burst_dropped"`
	Bandwidth     []BandwidthChange  `json:"bandwidth"`
	Profile       string             `json:"profile"`
}

func GenerateNetworkPlan(data []byte, seed int64, profile string) NetworkPlan {
	const packetSize = 4096
	stream := "studio-feed-" + profile
	var packets []transport.Packet
	for seq, off := 0, 0; off < len(data); seq, off = seq+1, off+packetSize {
		end := off + packetSize
		if end > len(data) {
			end = len(data)
		}
		packets = append(packets, transport.NewPacket(stream, uint64(seq), int64(off), data[off:end]))
	}
	n := len(packets)
	r := rand.New(rand.NewSource(seed + 77))
	burstStart := n / 3
	dropped := map[int]bool{burstStart: true, burstStart + 1: true, burstStart + 2: true}
	plan := NetworkPlan{
		StreamID:      stream,
		PacketSize:    packetSize,
		Packets:       packets,
		BurstDropped:  []int{burstStart, burstStart + 1, burstStart + 2},
		DuplicateSeqs: []int{0, min(9, n-1)},
		Profile:       profile,
	}
	for i := 0; i < n; i++ {
		if !dropped[i] {
			plan.SendOrder = append(plan.SendOrder, i)
		}
	}
	// Deterministic adjacent swaps create bounded one-packet reorder gaps.
	for _, frac := range []float64{0.05, 0.18, 0.55, 0.78} {
		pos := int(float64(len(plan.SendOrder)-2) * frac)
		if pos >= 0 && pos+1 < len(plan.SendOrder) {
			plan.SendOrder[pos], plan.SendOrder[pos+1] = plan.SendOrder[pos+1], plan.SendOrder[pos]
		}
	}
	if profile == "variable" {
		plan.Bandwidth = []BandwidthChange{
			{AfterPackets: n / 5, Bps: 512_000},
			{AfterPackets: 2 * n / 5, Bps: 2_000_000},
			{AfterPackets: 3 * n / 5, Bps: 12_000_000},
		}
	} else {
		bps := int64(4_000_000 + r.Intn(1_000_000))
		plan.Bandwidth = []BandwidthChange{{AfterPackets: 0, Bps: bps}}
	}
	return plan
}

// RunNetworkPlan drives the real transport assembler through initial sends,
// duplicates, bandwidth changes and retransmissions.
func RunNetworkPlan(plan NetworkPlan) (transport.Stats, []byte, error) {
	cfg := transport.DefaultConfig(plan.StreamID)
	cfg.MaxPacketSize = plan.PacketSize
	cfg.MaxRetries = 5
	a := transport.NewAssembler(cfg)
	bwIdx := 0
	for sent, seq := range plan.SendOrder {
		for bwIdx < len(plan.Bandwidth) && sent >= plan.Bandwidth[bwIdx].AfterPackets {
			a.SetBandwidth(plan.Bandwidth[bwIdx].Bps)
			bwIdx++
		}
		if err := a.Handle(plan.Packets[seq]); err != nil {
			return a.Stats(), nil, err
		}
	}
	for _, seq := range plan.DuplicateSeqs {
		if seq >= 0 && seq < len(plan.Packets) {
			if err := a.Handle(plan.Packets[seq]); err != nil {
				return a.Stats(), nil, err
			}
		}
	}
	for round := 0; round < cfg.MaxRetries; round++ {
		req, err := a.RetransmitRequests()
		if err != nil {
			return a.Stats(), nil, err
		}
		if len(req) == 0 {
			break
		}
		a.SetBandwidth(12_000_000)
		for _, seq64 := range req {
			seq := int(seq64)
			p := plan.Packets[seq]
			p.Retransmission = true
			if err := a.Handle(p); err != nil {
				return a.Stats(), nil, err
			}
		}
	}
	data, err := a.Assemble(len(plan.Packets))
	if err != nil {
		return a.Stats(), nil, err
	}
	return a.Stats(), data, nil
}

func VerifyAssembled(original, assembled []byte) error {
	if !bytes.Equal(original, assembled) {
		return model.New(model.CodeIntegrity, model.StageTransport, "assembled stream SHA/content differs from source")
	}
	return nil
}
