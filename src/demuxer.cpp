#include "media/demuxer.hpp"
#include <fstream>
#include <sstream>

namespace media {
namespace {
MediaType parseType(const std::string& s) {
  return s == "audio" ? MediaType::Audio : MediaType::Video;
}
} // namespace

// 侧车文本：
//   META tb_num tb_den sample_rate expected_packets
//   PKT ordinal track type pts duration keyframe(0|1) corrupt(0|1) offset
bool loadSidecar(const std::string& path, Sidecar& out, std::string& err) {
  std::ifstream f(path);
  if (!f) {
    err = "cannot open sidecar: " + path;
    return false;
  }
  std::string line;
  size_t lineNo = 0;
  while (std::getline(f, line)) {
    ++lineNo;
    if (line.empty() || line.starts_with('#')) continue;
    std::istringstream iss(line);
    std::string tag;
    iss >> tag;
    if (tag == "META") {
      if (!(iss >> out.timeBase.num >> out.timeBase.den >> out.sampleRate
                >> out.expectedPackets)) {
        err = "bad META at line " + std::to_string(lineNo);
        return false;
      }
    } else if (tag == "PKT") {
      SidecarEntry e;
      std::string type, key, corrupt;
      if (!(iss >> e.ordinal >> e.trackId >> type >> e.rawPts >>
                e.rawDuration >> key >> corrupt >> e.offset)) {
        err = "bad PKT at line " + std::to_string(lineNo);
        return false;
      }
      e.type = parseType(type);
      e.keyframe = key == "1";
      e.corrupt = corrupt == "1";
      out.entries.push_back(e);
    } else {
      err = "unknown sidecar tag: " + tag;
      return false;
    }
  }
  return true;
}

DemuxResult demuxTrack(const std::string& trackId, const Sidecar& sc,
                       int truncateAt) {
  DemuxResult r;
  size_t produced = 0;
  for (const auto& e : sc.entries) {
    if (e.trackId != trackId) continue;
    if (truncateAt > 0 && produced >= size_t(truncateAt)) {
      // 模拟容器截断：停止供包并上报致命错误与首个坏包位置。
      r.fatal = TimeError::TruncatedInput;
      r.firstBadPacket = e.ordinal;
      r.events.push_back({e.ordinal, e.rawPts, 0, TimeError::TruncatedInput,
                          "input truncated at packet " +
                              std::to_string(truncateAt)});
      break;
    }
    MediaPacket p;
    p.type = e.type;
    p.index = produced;
    p.globalIndex = e.ordinal;
    p.rawPts = e.rawPts;
    p.rawDts = e.rawPts;
    p.durationTicks = e.rawDuration;
    p.keyframe = e.keyframe;
    p.corrupt = e.corrupt;
    if (e.corrupt)
      r.events.push_back({e.ordinal, e.rawPts, 0, TimeError::CorruptPacket,
                          "flagged corrupt in sidecar"});
    r.packets.push_back(std::move(p));
    ++produced;
  }
  if (r.packets.empty() && r.fatal == TimeError::None)
    r.fatal = TimeError::ZeroDuration;
  return r;
}
} // namespace media
