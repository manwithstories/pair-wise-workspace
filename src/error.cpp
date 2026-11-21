#include "media/error.hpp"

#include <sstream>

namespace media {

const char* toString(ErrorCode code) {
  switch (code) {
    case ErrorCode::None: return "none";
    case ErrorCode::InvalidArgument: return "invalid_argument";
    case ErrorCode::IoFailure: return "io_failure";
    case ErrorCode::BadRiffHeader: return "bad_riff_header";
    case ErrorCode::UnsupportedFormat: return "unsupported_format";
    case ErrorCode::TruncatedChunk: return "truncated_chunk";
    case ErrorCode::BadChunkLength: return "bad_chunk_length";
    case ErrorCode::NonIntegerFrameTail: return "non_integer_frame_tail";
    case ErrorCode::CheckpointMismatch: return "checkpoint_mismatch";
    case ErrorCode::CheckpointCorrupt: return "checkpoint_corrupt";
    case ErrorCode::EncodeFailed: return "encode_failed";
    case ErrorCode::ValidationFailed: return "validation_failed";
  }
  return "unknown";
}

const char* toString(Stage stage) {
  switch (stage) {
    case Stage::Read: return "read";
    case Stage::Segment: return "segment";
    case Stage::Silence: return "silence";
    case Stage::Fingerprint: return "fingerprint";
    case Stage::Transcode: return "transcode";
    case Stage::Checkpoint: return "checkpoint";
    case Stage::Manifest: return "manifest";
    case Stage::Verify: return "verify";
  }
  return "unknown";
}

std::string MediaError::what() const {
  std::ostringstream os;
  os << "[" << toString(stage) << "/" << toString(code) << "]";
  if (!sourcePath.empty()) os << " " << sourcePath;
  if (byteOffset >= 0) os << " byte_offset=" << byteOffset;
  if (sampleIndex >= 0) os << " sample_index=" << sampleIndex;
  if (!message.empty()) os << ": " << message;
  return os.str();
}

}  // namespace media
