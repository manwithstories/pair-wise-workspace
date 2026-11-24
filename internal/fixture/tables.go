package fixture

// stbl assembles the sample table; its children are added next.
func (b *builder) stbl(lay trackLayout, mdatStart int64) []byte {
	return containerBox("stbl")
}
