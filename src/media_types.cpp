#include "media_types.hpp"

namespace mfs {

const char* to_string(CodecId codec) {
  switch (codec) {
    case CodecId::Png:
      return "png";
    case CodecId::Mjpeg:
      return "mjpeg";
  }
  return "unknown";
}

const char* to_string(ErrorType type) {
  switch (type) {
    case ErrorType::FileHeaderTruncated:
      return "file_header_truncated";
    case ErrorType::FileHeaderInvalid:
      return "file_header_invalid";
    case ErrorType::UnsupportedFileVersion:
      return "unsupported_file_version";
    case ErrorType::RecordTruncated:
      return "record_truncated";
    case ErrorType::RecordMagicMismatch:
      return "record_magic_mismatch";
    case ErrorType::LengthOverflow:
      return "length_overflow";
    case ErrorType::ChecksumMismatch:
      return "checksum_mismatch";
    case ErrorType::UnknownRequiredField:
      return "unknown_required_field";
    case ErrorType::UnsupportedMetadata:
      return "unsupported_metadata";
    case ErrorType::OutOfWindow:
      return "out_of_window";
    case ErrorType::DecodeError:
      return "decode_error";
    case ErrorType::LimitExceeded:
      return "limit_exceeded";
    case ErrorType::EmptySequence:
      return "empty_sequence";
    case ErrorType::RoundtripMismatch:
      return "roundtrip_mismatch";
    case ErrorType::SegmentLimit:
      return "segment_limit";
  }
  return "unknown";
}

}  // namespace mfs
