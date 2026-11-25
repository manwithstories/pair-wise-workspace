package manifest

// TrackMetadata pairs the primary track description with zero or more
// additional reconciliation hints (container, sidecar, manifest, ...).
type TrackMetadata struct {
	Track TrackDesc      `json:"track"`
	Hints []MetadataHint `json:"hints,omitempty"`
}

// MetadataFile is the on-disk track metadata sidecar.
type MetadataFile struct {
	Tracks []TrackMetadata `json:"tracks"`
}
