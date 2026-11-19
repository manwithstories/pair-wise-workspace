#pragma once
// 剪辑清单：机位、轨道、剪辑点、交叉淡化转场。
// 清单文本格式（行导向，# 开头为注释）：
//   TRACK <id> <video|audio> <file> <tb_num> <tb_den>
//   EDIT   <n> <track> <target_ms>
//   XFADE  <n> <from_track> <to_track> <start_ms> <end_ms>
#include <string>
#include <vector>
#include "media/packet.hpp"
#include "media/time_model.hpp"

namespace media {

struct TrackSpec {
  std::string id;
  MediaType type = MediaType::Video;
  std::string file;
  Rational timeBase;
};

struct EditPoint {
  size_t index = 0;
  std::string trackId;
  int64_t targetUs = 0;
};

struct Manifest {
  std::vector<TrackSpec> tracks;
  std::vector<EditPoint> edits;
  std::vector<Transition> transitions;
  const TrackSpec* findTrack(const std::string& id) const;
};

struct ManifestError {
  size_t line = 0;
  std::string message;
  explicit operator bool() const { return !message.empty(); }
};

// 解析清单；失败时返回错误（行号 + 原因），Manifest 保持部分填充。
ManifestError parseManifest(const std::string& text, Manifest& out);
std::string manifestToText(const Manifest& m);

// 把清单剪辑点对齐到某轨实际包时间线，返回边界状态（最近有效帧 + 误差）。
BoundaryState alignEditPoint(const EditPoint& edit,
                             const std::vector<MediaPacket>& packets);
} // namespace media
