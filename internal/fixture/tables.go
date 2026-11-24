package fixture

// stbl and buildMoovAt are added with the header writers that fill them.
func (b *builder) buildMoovAt(mdatPayloadStart int64) []byte {
	return containerBox("moov")
}
