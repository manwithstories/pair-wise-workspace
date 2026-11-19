#include "media_error.hpp"

namespace mediaqc {

const char* stage_name(Stage s) {
  switch (s) {
    case Stage::Open: return "open";
    case Stage::Structure: return "structure";
    case Stage::Scan: return "scan";
    case Stage::Timeline: return "timeline";
    case Stage::Write: return "write";
    case Stage::Index: return "index";
    case Stage::Verify: return "verify";
  }
  return "unknown";
}

const char* severity_name(Severity s) {
  switch (s) {
    case Severity::Recovered: return "recovered";
    case Severity::Fatal: return "fatal";
  }
  return "unknown";
}

const char* code_name(Code c) {
  switch (c) {
    case Code::None: return "none";
    case Code::FileEmpty: return "file_empty";
    case Code::HeaderUnparsable: return "header_unparsable";
    case Code::NoMoov: return "no_moov";
    case Code::MoovTruncated: return "moov_truncated";
    case Code::BoxTruncated: return "box_truncated";
    case Code::MdatTruncated: return "mdat_truncated";
    case Code::PacketTruncated: return "packet_truncated";
    case Code::PacketOutOfBounds: return "packet_out_of_bounds";
    case Code::IndexEntryOutOfRange: return "index_entry_out_of_range";
    case Code::IndexRebuiltFromSizes: return "index_rebuilt_from_sizes";
    case Code::IndexMissing: return "index_missing";
    case Code::DecodeError: return "decode_error";
    case Code::DecodeDependentDropped: return "decode_dependent_dropped";
    case Code::TimestampMissing: return "timestamp_missing";
    case Code::TimestampBackwards: return "timestamp_backwards";
    case Code::TimestampWrap: return "timestamp_wrap";
    case Code::TimestampAmbiguous: return "timestamp_ambiguous";
    case Code::TrackLimitExceeded: return "track_limit_exceeded";
    case Code::PacketTooLarge: return "packet_too_large";
    case Code::FileTooLarge: return "file_too_large";
    case Code::PathEscape: return "path_escape";
    case Code::UnsupportedCodec: return "unsupported_codec";
    case Code::OutputWriteFailed: return "output_write_failed";
    case Code::OutputVerifyFailed: return "output_verify_failed";
    case Code::CheckpointMismatch: return "checkpoint_mismatch";
    case Code::CheckpointCorrupt: return "checkpoint_corrupt";
    case Code::EmptyOutput: return "empty_output";
    case Code::UnindexableRange: return "unindexable_range";
    case Code::OffsetUnknown: return "offset_unknown";
  }
  return "unknown";
}

}  // namespace mediaqc
