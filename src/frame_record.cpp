#include "relay/frame_record.hpp"

// Skeleton: implementation lands in a follow-up commit.
namespace relay {

bool is_known_time_base(const TimeBase&) { return false; }
std::string to_string(const TimeBase&) { return {}; }
const char* to_string(ParseError) { return "unimplemented"; }
Sha256 sha256(const uint8_t*, size_t) { return {}; }
Sha256 record_checksum(const FrameRecord&) { return {}; }
bool checksum_matches(const FrameRecord&) { return false; }
std::string hex(const Sha256&) { return {}; }

FrameContainerWriter::FrameContainerWriter(const std::string&) {}
FrameContainerWriter::~FrameContainerWriter() = default;
bool FrameContainerWriter::write_header(uint64_t) { return false; }
bool FrameContainerWriter::write_record(const FrameRecord&) { return false; }
bool FrameContainerWriter::write_trailer() { return false; }
bool FrameContainerWriter::good() const { return false; }

FrameContainerReader::FrameContainerReader(const std::string&) {}
FrameContainerReader::~FrameContainerReader() = default;
ParseResult FrameContainerReader::open() { return {ParseError::BadMagic, 0, "unimplemented"}; }
bool FrameContainerReader::next(FrameRecord&) { return false; }

} // namespace relay
