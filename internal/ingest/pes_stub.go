package ingest

func (s *Scanner) appendPES(pid uint16, pusi bool, payload []byte, off int64, packetIndex int) (Record, bool, error) {
	return Record{}, false, nil
}

func (s *Scanner) Flush() []Record { return nil }
