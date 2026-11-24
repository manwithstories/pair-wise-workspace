package mp4

import (
	"encoding/binary"
)

func parseMfhd(data []byte, h BoxHeader) (uint32, error) {
	_, _, p, err := ReadFullBox(data, h)
	if err != nil {
		return 0, err
	}
	if err := readNeed(data, p, h.End, 4, "mfhd sequence"); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(data[p : p+4]), nil
}

func parseTfhdTrack(data []byte, h BoxHeader) (uint32, error) {
	_, _, p, err := ReadFullBox(data, h)
	if err != nil {
		return 0, err
	}
	if err := readNeed(data, p, h.End, 4, "tfhd track id"); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(data[p : p+4]), nil
}

func parseTfdt(data []byte, h BoxHeader) (uint64, byte, error) {
	ver, _, p, err := ReadFullBox(data, h)
	if err != nil {
		return 0, 0, err
	}
	switch ver {
	case 1:
		if err := readNeed(data, p, h.End, 8, "tfdt v1 time"); err != nil {
			return 0, 0, err
		}
		return binary.BigEndian.Uint64(data[p : p+8]), 1, nil
	default:
		if err := readNeed(data, p, h.End, 4, "tfdt v0 time"); err != nil {
			return 0, 0, err
		}
		return uint64(binary.BigEndian.Uint32(data[p : p+4])), 0, nil
	}
}
