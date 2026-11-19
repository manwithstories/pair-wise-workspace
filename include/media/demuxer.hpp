#pragma once
// 合成素材解封装：读取生成器产出的 MKV/WAV 与侧车真值，转换为 MediaPacket 流。
#include <functional>
#include <string>
#include <vector>
#include "media/packet.hpp"
#include "media/time_model.hpp"

namespace media {

// 侧车真值条目（生成器写入，解封装器读取），用于注入故障的确定性复现。
struct SidecarEntry {
  size_t ordinal = 0;       // 全轨累计包序号
  std::string trackId;
  MediaType type = MediaType::Video;
  int64_t rawPts = 0;
  int64_t rawDuration = 0;
  int64_t linearPts = 0;    // 仅含漂移、剥离 VFR 的线性化 PTS（漂移锚点用）
  bool keyframe = false;
  bool corrupt = false;
  uint64_t offset = 0;
};

struct Sidecar {
  Rational timeBase;
  int sampleRate = 0;
  size_t expectedPackets = 0;
  std::vector<SidecarEntry> entries;
};

struct DemuxResult {
  std::vector<MediaPacket> packets;
  std::vector<TimeEvent> events;
  TimeError fatal = TimeError::None;  // 截断等致命错误
  size_t firstBadPacket = 0;
};

// 解析侧车文本（"track ord type pts dur key corrupt offset"）。
bool loadSidecar(const std::string& path, Sidecar& out, std::string& err);

// 按侧车把容器解封装为包流；truncateAt>0 时在该序号处模拟截断。
DemuxResult demuxTrack(const std::string& trackId, const Sidecar& sc,
                       int truncateAt);
} // namespace media
