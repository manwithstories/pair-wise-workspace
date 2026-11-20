#include "mediacore/time.hpp"
#include "mediacore/pcm.hpp"

#include <sstream>

namespace mediacore {

std::string toString(SampleRange range) {
    std::ostringstream os;
    os << '[' << range.start << ',' << range.end << ')';
    return os.str();
}

const char* blockKindName(BlockKind kind) noexcept {
    switch (kind) {
    case BlockKind::Normal:         return "normal";
    case BlockKind::IncompleteTail: return "incomplete_tail";
    case BlockKind::Corrupt:        return "corrupt";
    case BlockKind::Gap:            return "gap";
    case BlockKind::Duplicate:      return "duplicate";
    }
    return "unknown";
}

const char* errorCodeName(MediaErrorCode code) noexcept {
    switch (code) {
    case MediaErrorCode::Ok:                  return "ok";
    case MediaErrorCode::InvalidArgument:     return "invalid_argument";
    case MediaErrorCode::PathTraversal:       return "path_traversal";
    case MediaErrorCode::UnsupportedFormat:   return "unsupported_format";
    case MediaErrorCode::TruncatedHeader:     return "truncated_header";
    case MediaErrorCode::CorruptStream:       return "corrupt_stream";
    case MediaErrorCode::IncompleteTail:      return "incomplete_tail";
    case MediaErrorCode::MissingIndex:        return "missing_index";
    case MediaErrorCode::ScanLimitExceeded:   return "scan_limit_exceeded";
    case MediaErrorCode::DuplicateBlock:      return "duplicate_block";
    case MediaErrorCode::TimestampGap:        return "timestamp_gap";
    case MediaErrorCode::MappingOutOfRange:   return "mapping_out_of_range";
    case MediaErrorCode::MappingDuplicate:     return "mapping_duplicate";
    case MediaErrorCode::MappingIncompatible: return "mapping_incompatible";
    case MediaErrorCode::WriteFailed:         return "write_failed";
    case MediaErrorCode::Cancelled:           return "cancelled";
    }
    return "unknown";
}

std::size_t truncateToFrameBoundary(std::vector<std::uint8_t>& buffer,
                                    const PcmFormat& format) noexcept {
    const int frame = format.frameBytes();
    if (frame <= 0) {
        const std::size_t drop = buffer.size();
        buffer.clear();
        return drop;
    }
    const std::size_t keep = (buffer.size() / frame) * frame;
    const std::size_t dropped = buffer.size() - keep;
    buffer.resize(keep);
    return dropped;
}

} // namespace mediacore
