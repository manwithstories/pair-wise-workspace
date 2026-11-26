// Package fixture generates all local demo media and deterministic network
// events in Go; no external media files are referenced.
package fixture

// ProofAssets is one reproducible source media plus its weak-network event
// plan.
type ProofAssets struct {
	Media   *Media
	Network NetworkPlan
}

// GenerateProofAssets creates the multi-codec media and matching duplicate,
// reorder, burst-loss and bandwidth events from one seed.
func GenerateProofAssets(seed int64, profile string) (*ProofAssets, error) {
	media, err := GenerateMedia(seed, nil)
	if err != nil {
		return nil, err
	}
	return &ProofAssets{
		Media:   media,
		Network: GenerateNetworkPlan(media.Data, seed, profile),
	}, nil
}
