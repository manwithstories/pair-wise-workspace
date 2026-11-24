package parse

import (
	mp4 "github.com/abema/go-mp4"
)

// encodeFtyp writes the file type box for the brands this pipeline emits.
func encodeFtyp() []byte {
	compat := []mp4.CompatibleBrandElem{
		{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
		{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
	}
	body, err := marshalBody(&mp4.Ftyp{
		MajorBrand:       majorBrand,
		MinorVersion:     uint32(minorBrand),
		CompatibleBrands: compat,
	})
	if err != nil {
		return nil
	}
	return leaf("ftyp", body).encode()
}
