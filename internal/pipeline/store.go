package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"relayctl/internal/fixture"
)

// On-disk file names inside a fixture directory.
const (
	manifestFile = "fixture.json"
	cmafFile     = "stream.cmaf"
	damagedFile  = "stream.damaged.cmaf"
	wireFile     = "wire.rtp.json"
	outManifest  = "manifest.json"
)

// SaveScenario writes a generated scenario to dir as stable local files.
func SaveScenario(dir string, sc *fixture.Scenario) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pipeline: create fixture dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, cmafFile), sc.CMAF, 0o644); err != nil {
		return err
	}
	if sc.DamagedCMAF != nil {
		if err := os.WriteFile(filepath.Join(dir, damagedFile), sc.DamagedCMAF, 0o644); err != nil {
			return err
		}
	}
	meta, err := json.MarshalIndent(sc.Manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	wire, err := json.MarshalIndent(encodeWire(sc), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, wireFile), append(wire, '\n'), 0o644)
}

// LoadScenario reads a fixture directory back into a runnable scenario.
func LoadScenario(dir string) (*fixture.Scenario, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return nil, fmt.Errorf("pipeline: read fixture manifest: %w", err)
	}
	var man fixture.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, fmt.Errorf("pipeline: parse fixture manifest: %w", err)
	}
	cmafBytes, err := os.ReadFile(filepath.Join(dir, cmafFile))
	if err != nil {
		return nil, err
	}
	sc := &fixture.Scenario{Manifest: man, CMAF: cmafBytes}
	if damaged, derr := os.ReadFile(filepath.Join(dir, damagedFile)); derr == nil {
		sc.DamagedCMAF = damaged
	}
	return sc, nil
}

// wirePacket is the JSON-serializable view of one RTP packet.
type wirePacket struct {
	Seq       uint16 `json:"seq"`
	IsFEC     bool   `json:"is_fec"`
	SSRC      uint32 `json:"ssrc"`
	Timestamp uint32 `json:"timestamp"`
	Payload   []byte `json:"payload"`
}

func encodeWire(sc *fixture.Scenario) []wirePacket {
	out := make([]wirePacket, 0, len(sc.Packets))
	for _, p := range sc.Packets {
		out = append(out, wirePacket{
			Seq: p.Seq, IsFEC: p.IsFEC, SSRC: p.Packet.SSRC,
			Timestamp: p.Packet.Timestamp, Payload: p.Packet.Payload,
		})
	}
	return out
}
