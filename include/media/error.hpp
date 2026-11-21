#pragma once

#include <cstdint>
#include <string>

namespace media {

// 损坏素材处置策略：跳过可恢复块继续处理，或立即终止。
enum class CorruptionPolicy { Skip, Abort };

enum class ErrorCode {
  None,
  InvalidArgument,
  IoFailure,
  BadRiffHeader,
  UnsupportedFormat,
  TruncatedChunk,
  BadChunkLength,
  NonIntegerFrameTail,
  CheckpointMismatch,
  CheckpointCorrupt,
  EncodeFailed,
  ValidationFailed,
};

enum class Stage {
  Read,
  Segment,
  Silence,
  Fingerprint,
  Transcode,
  Checkpoint,
  Manifest,
  Verify,
};

const char* toString(ErrorCode code);
const char* toString(Stage stage);

// 带源路径、字节偏移、样本序号与阶段的结构化错误。
struct MediaError {
  ErrorCode code = ErrorCode::None;
  Stage stage = Stage::Read;
  std::string sourcePath;
  int64_t byteOffset = -1;   // 文件字节偏移；不适用时为 -1
  int64_t sampleIndex = -1;  // 采样帧序号；不适用时为 -1
  std::string message;
  bool recoverable = false;

  bool ok() const { return code == ErrorCode::None; }
  std::string what() const;
};

}  // namespace media
