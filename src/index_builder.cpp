#include "media/index_builder.hpp"
#include <algorithm>
#include <cstdlib>
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
  // 双向最近邻：lower_bound 找首个不早于目标的关键帧，再与前一个比较，
  // 选落点误差绝对值更小者，把 seek 误差压到半个关键帧间隔以内。
  auto next = std::lower_bound(
      entries_.begin(), entries_.end(), targetUs,
      [](const IndexEntry& e, int64_t t) { return e.ptsUs < t; });
  auto best = next;
  if (next == entries_.begin()) {
    best = entries_.begin(); // 目标早于首关键帧
  } else if (next == entries_.end()) {
    best = entries_.end() - 1; // 目标晚于末关键帧
  } else {
    auto prev = next - 1;
    best = std::llabs(prev->ptsUs - targetUs) <=
                   std::llabs(next->ptsUs - targetUs)
               ? prev
               : next;
  }
  SeekResult r;
  r.targetUs = targetUs;
  r.entry = *best;
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
