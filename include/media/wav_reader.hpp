#pragma once

#include <cstdint>
#include <fstream>
#include <functional>
#include <string>
#include <vector>

#include "media/audio.hpp"
#include "media/error.hpp"

namespace media {

// 解析结束后的输入摘要：检查点据此拒绝不匹配的续跑。
struct WavSummary {
  std::string path;
  uint64_t fileSize = 0;
  uint64_t digest = 0;        // FNV-1a，覆盖整个文件字节
  AudioFormat format;
  SampleTick totalFrames = 0; // 成功交付的完整帧数（不含非整帧尾块）
  uint64_t dataBytes = 0;     // data 块声明/实际字节合计
  uint64_t tailBytes = 0;     // 非整帧尾块字节数
};

// 每批音频帧回调；返回 false 可中止读取（不算损坏错误）。
using FrameCallback = std::function<bool(const AudioBlock& block)>;

class WavReader {
 public:
  static constexpr size_t kDefaultBlockFrames = 4096;

  WavReader(std::string path, CorruptionPolicy policy,
            size_t blockFrames = kDefaultBlockFrames);

  // 流式解析整个文件；onFrame 可为空（仅做解析与摘要）。
  // 致命错误返回非 ok()；可恢复错误累积在 recoverableErrors()。
  MediaError read(const FrameCallback& onFrame);

  const WavSummary& summary() const { return summary_; }
  const std::vector<MediaError>& recoverableErrors() const {
    return recoverable_;
  }
  bool hadRecoverable() const { return !recoverable_.empty(); }

 private:
  struct ChunkInfo {
    char id[4];
    uint64_t size = 0;     // 块声明长度（不含 8 字节头）
    uint64_t dataPos = 0;  // 数据起始文件偏移
  };

  bool readExact(void* dst, size_t n, MediaError* err);
  bool readU8(uint8_t* v);
  bool readU16(uint16_t* v);
  bool readU32(uint32_t* v);
  bool parseFmt(uint64_t chunkSize, MediaError* err);
  void mapEncoding();
  // *eofReached 在读取触达文件末尾时置位，调用方据此停止块扫描。
  bool readDataChunk(uint64_t chunkSize, const FrameCallback& cb,
                     MediaError* err, bool* eofReached);
  void decodeFrame(const unsigned char* raw, int32_t* out) const;
  MediaError makeError(ErrorCode code, int64_t byteOff, int64_t sample,
                       const std::string& msg, bool recoverable) const;
  bool handleCorruption(MediaError* err, const MediaError& candidate);

  std::string path_;
  CorruptionPolicy policy_;
  size_t blockFrames_;
  std::ifstream in_;
  WavSummary summary_;
  std::vector<MediaError> recoverable_;
  std::vector<int32_t> sampleBuffer_;
  SampleTick frameCursor_ = 0;
  bool fmtSeen_ = false;
};

}  // namespace media
