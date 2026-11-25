package pipeline

import (
	"github.com/pairwise/mediad/internal/fixture"
	"github.com/pairwise/mediad/internal/ingest"
	"github.com/pairwise/mediad/internal/manifest"
	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/repair"
)

// rejectDamageSamples exercises the material-corruption rejection gate. Link
// impairments have already been counted and recovered before this gate runs.
func rejectDamageSamples(b *fixture.Bundle) error {
	s := ingest.NewStream(8)
	if err := s.Accept(b.ConflictA); err != nil {
		return media.Wrap(err, media.CodeInternal, "pipeline.damage", "conflict-setup", "first conflict packet rejected unexpectedly")
	}
	if err := s.Accept(b.ConflictB); err == nil || media.ExitCode(err) != 2 {
		return media.NewError(media.CodeInternal, "pipeline.damage", "rtp-conflict", "conflict sample was not rejected")
	}

	if err := repair.ValidateBoxes(b.TruncatedBox); media.ExitCode(err) != 2 {
		return media.NewError(media.CodeInternal, "pipeline.damage", "truncated-box", "truncated box was not rejected")
	}
	if err := repair.ValidateBoxes(b.OversizeBox); media.ExitCode(err) != 4 {
		return media.NewError(media.CodeInternal, "pipeline.damage", "oversize-box", "oversize box was not rejected with argument/limit exit 4")
	}
	if _, err := manifest.ParseHLS(b.MissingFieldHLS); media.ExitCode(err) != 2 {
		return media.NewError(media.CodeInternal, "pipeline.damage", "missing-hls-field", "invalid HLS was not rejected")
	}
	if _, err := manifest.ParseDASH(b.MissingFieldDASH); media.ExitCode(err) != 2 {
		return media.NewError(media.CodeInternal, "pipeline.damage", "missing-dash-field", "invalid DASH was not rejected")
	}
	return media.NewError(media.CodeMalformed, "pipeline.damage", "material", "generated damaged samples rejected by contract")
}
