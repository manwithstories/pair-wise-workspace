#pragma once
// 关键帧随机访问索引：按时间定位、报告落点误差、原子写入（临时文件+完成标记）。
#include <cstdint>
#include <string>
#include <vector>
#include "media/packet.hpp"
#include "media/time_model.hpp"

namespace media {

struct IndexEntry {
  int64_t ptsUs = 0;     // 关键帧在统一时间基上的 PTS
  uint64_t offset = 0;   // 容器内字节偏移（合成素材中为包序号*估算，可复现）
  size_t packetIndex = 0;
};

struct SeekResult {
  IndexEntry entry;
  int64_t targetUs = 0;
  int64_t errorUs = 0;   // 实际落点 - 目标
};

class KeyframeIndex {
public:
  void build(const std::vector<MediaPacket>& packets);
  const std::vector<IndexEntry>& entries() const { return entries_; }

  // 定位不晚于 targetUs 的最近关键帧；无关键帧返回 nullopt。
  std::optional<SeekResult> seek(int64_t targetUs) const;

  // 原子写：先写 <path>.tmp，fsync 后 rename，再写完成标记 <path>.done。
  // 截断/缺失完成标记 -> IoError，绝不误报成功。
  TimeError writeAtomic(const std::string& path) const;
  static bool isComplete(const std::string& path);

private:
  std::vector<IndexEntry> entries_;
};
} // namespace media
