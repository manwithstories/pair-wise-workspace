#include "media/index_builder.hpp"
#include <algorithm>
#include <filesystem>
#include <fstream>
#include <sstream>

namespace media {
void KeyframeIndex::build(const std::vector<MediaPacket>& packets) {
  entries_.clear();
  for (const auto& p : packets) {
    if (!p.keyframe || !p.deliverable) continue;
    entries_.push_back({p.repairedUs,
                        uint64_t(p.globalIndex) * 1024, // 确定性伪偏移
                        p.index});
  }
}

std::optional<SeekResult> KeyframeIndex::seek(int64_t targetUs) const {
  if (entries_.empty()) return std::nullopt;
  // 取不晚于目标的最后一个关键帧（upper_bound - 1）。
  auto it = std::upper_bound(
      entries_.begin(), entries_.end(), targetUs,
      [](int64_t t, const IndexEntry& e) { return t < e.ptsUs; });
  if (it == entries_.begin()) {
    // 目标早于首个关键帧：回退到首帧，由误差字段暴露偏差。
    it = entries_.begin();
  } else {
    --it;
  }
  SeekResult r;
  r.targetUs = targetUs;
  r.entry = *it;
  r.errorUs = r.entry.ptsUs - targetUs;
  return r;
}

namespace {
std::string tmpPath(const std::string& p) { return p + ".tmp"; }
std::string donePath(const std::string& p) { return p + ".done"; }
} // namespace

TimeError KeyframeIndex::writeAtomic(const std::string& path) const {
  // 1) 写临时文件并 flush；2) rename 覆盖目标；3) 写完成标记。
  // 任一步失败都不留完成标记，读取方据此拒绝误报成功。
  const std::string tmp = tmpPath(path);
  {
    std::ofstream f(tmp, std::ios::trunc);
    if (!f) return TimeError::IoError;
    f << "KEYFRAME_INDEX v1 count=" << entries_.size() << '\n';
    for (const auto& e : entries_)
      f << e.ptsUs << ' ' << e.offset << ' ' << e.packetIndex << '\n';
    f.flush();
    if (!f) return TimeError::IoError;
  }
  std::error_code ec;
  std::filesystem::rename(tmp, path, ec);
  if (ec) return TimeError::IoError;
  {
    std::ofstream done(donePath(path), std::ios::trunc);
    if (!done) return TimeError::IoError;
    done << "complete\n";
    done.flush();
    if (!done) return TimeError::IoError;
  }
  return TimeError::None;
}

bool KeyframeIndex::isComplete(const std::string& path) {
  const std::string done = donePath(path);
  std::ifstream f(done);
  if (!f) return false;
  std::string token;
  return bool(f >> token) && token == "complete";
}
} // namespace media
