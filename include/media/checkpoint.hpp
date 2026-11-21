#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/audio.hpp"
#include "media/error.hpp"
#include "media/json.hpp"
#include "media/time.hpp"

namespace media {

// 续跑必须核对的输入摘要与作业参数。
struct CheckpointParams {
  std::string inputPath;
  std::string outputPath;
  uint64_t inputDigest = 0;
  uint64_t inputFileSize = 0;
  uint32_t sampleRate = 0;
  uint16_t channels = 0;
  uint16_t bitsPerSample = 0;
  SampleTick totalFrames = 0;
  int64_t targetBitrate = 0;
  uint32_t targetSampleRate = 0;
  uint16_t targetChannels = 0;
  int64_t segmentMaxSamples = 0;
  int64_t silenceThreshold = 0;
};

struct CheckpointState {
  int schemaVersion = 1;
  CheckpointParams params;
  int completedSegments = 0;       // 已完成处理的片段数
  std::vector<int> duplicateIds;   // 判重跳过的片段
  bool transcodeDone = false;
  bool manifestDone = false;
  uint64_t outputBytes = 0;
  SampleTick outputFrames = 0;
};

class CheckpointStore {
 public:
  explicit CheckpointStore(std::string path);

  // 阶段完成后原子写入；失败返回带阶段信息的错误。
  MediaError save(const CheckpointState& state) const;
  // 读取检查点；文件不存在时返回 ok 且 *exists=false。
  MediaError load(CheckpointState* state, bool* exists) const;
  // 核对输入摘要与参数：不匹配拒绝续跑。
  static MediaError validate(const CheckpointState& state,
                             const CheckpointParams& expected);

  static json::Value toJson(const CheckpointState& state);
  static bool fromJson(const json::Value& v, CheckpointState* state,
                       std::string* error);

 private:
  std::string path_;
};

}  // namespace media
