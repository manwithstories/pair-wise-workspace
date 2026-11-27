package fixture

import (
	"mediad/internal/transport"
)

// LoadProfileForTest exposes the transport fault-profile loader to fixture
// tests without coupling the generator to JSON decoding details.
func LoadProfileForTest(path string) (*transport.FaultProfile, error) {
	return transport.LoadFaultProfile(path)
}
