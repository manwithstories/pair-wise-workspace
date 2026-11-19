#include "media/error_model.hpp"

namespace media {

const char* to_string(ErrorKind kind) {
    switch (kind) {
    case ErrorKind::Duplicate: return "duplicate";
    case ErrorKind::OutOfOrder: return "out_of_order";
    case ErrorKind::LateOutsideWindow: return "late_outside_window";
    case ErrorKind::Lost: return "lost";
    case ErrorKind::Recovered: return "recovered";
    case ErrorKind::Unrecoverable: return "unrecoverable";
    case ErrorKind::CorruptRecord: return "corrupt_record";
    case ErrorKind::TruncatedRecord: return "truncated_record";
    case ErrorKind::TimestampWrap: return "timestamp_wrap";
    case ErrorKind::DuplicateTimestamp: return "duplicate_timestamp";
    case ErrorKind::UnconvertibleTimecode: return "unconvertible_timecode";
    case ErrorKind::CheckpointCorrupt: return "checkpoint_corrupt";
    case ErrorKind::WriteFailure: return "write_failure";
    case ErrorKind::Uncertain: return "uncertain";
    }
    return "uncertain";
}

void ErrorModel::record(ErrorRecord rec) {
    counts_[static_cast<size_t>(rec.kind)]++;
    if (records_.size() < kMaxRecords) {
        records_.push_back(std::move(rec));
    } else {
        dropped_++;
    }
}

uint64_t ErrorModel::count_of(ErrorKind kind) const {
    return counts_[static_cast<size_t>(kind)];
}

} // namespace media
