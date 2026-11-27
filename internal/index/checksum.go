package index

import (
	"hash/crc32"

	"avrepair/internal/clock"
)

// crc32Check validates a framed entry against its stored IEEE checksum.
func crc32Check(frame []byte, want uint32) error {
	if got := crc32.ChecksumIEEE(frame); got != want {
		return clock.NewError(clock.CodeChecksum, "", clock.Interval{},
			"stored checksum does not match entry bytes")
	}
	return nil
}
