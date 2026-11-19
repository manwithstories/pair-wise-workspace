#include "media/pipeline.hpp"
#include "media/clock_repair.hpp"
#include "media/decoder.hpp"
#include "media/index_builder.hpp"
#include "media/stream_pipeline.hpp"
#include <cmath>
#include <filesystem>
#include <fstream>
#include <sstream>

namespace media {

bool loadManifestAndSidecars(const std::string& dir,
                             const std::string& manifestPath, Manifest& m,
                             std::map<std::string, Sidecar>& cars,
                             std::string& err) {
  std::ifstream mf(manifestPath);
  if (!mf) {
    err = "cannot open manifest: " + manifestPath;
    return false;
  }
  std::string text((std::istreambuf_iterator<char>(mf)),
                   std::istreambuf_iterator<char>());
  if (auto e = parseManifest(text, m); e) {
    err = "manifest line " + std::to_string(e.line) + ": " + e.message;
    return false;
  }
  for (const auto& t : m.tracks) {
    Sidecar sc;
    std::string scErr;
    std::string p = dir + "/" + t.id + ".sidecar";
    if (!loadSidecar(p, sc, scErr)) {
      err = scErr;
      return false;
    }
    cars[t.id] = std::move(sc);
  }
  return true;
}

namespace {
// 单轨：解封装 -> 修复 + 锚点 -> 有界队列背压 -> 索引/对齐。
TimeError processTrack(const TrackSpec& spec, const Sidecar& sc,
                       const PipelineConfig& cfg, const Manifest& manifest,
                       TrackResult& out, size_t& queuePeak) {
  auto dem = demuxTrack(spec.id, sc, cfg.truncateAt);
  if (isFatal(dem.fatal)) {
    out.repair.firstBadPacket = dem.firstBadPacket;
    return dem.fatal;
  }
  ClockRepair repair(spec.timeBase, spec.type);
  for (auto& pkt : dem.packets)
    repair.accept(pkt);
  // 漂移锚点：实测轴用侧车提供的线性化时间（仅含晶振漂移、剥离 VFR），
  // 参考轴用理想时间（视频 i/30fps、音频按采样计数）。两者呈干净线性关系，
  // 拟合斜率即真实晶振漂移；校正后再叠加回 VFR 抖动，对齐误差被压到一帧内。
  for (const auto& pkt : dem.packets) {
    double refS = spec.type == MediaType::Video
                      ? double(pkt.index) / 30.0
                      : double(pkt.rawPts) / double(spec.timeBase.den);
    double measS = double(pkt.linearPts) * spec.timeBase.num /
                   double(spec.timeBase.den);
    repair.addAnchor(measS, refS);
  }
  auto drift = repair.estimateDrift();
  applyDrift(dem.packets, drift);
  out.repair = repair.summary();
  out.repair.drift = drift;

  // 有界队列：队满背压；损坏包作为可恢复事件，不取消管线。
  PacketQueue q(cfg.queuePackets);
  std::vector<TimeEvent> diag;
  auto stats = runPipeline(q, dem.packets, [&](MediaPacket& p) {
    if (p.corrupt)
      diag.push_back({p.globalIndex, p.rawPts, p.repairedUs,
                      TimeError::CorruptPacket, "corrupt packet"});
    return TimeError::None;
  });
  if (isFatal(stats.error)) return stats.error;
  queuePeak = std::max(queuePeak, stats.peak);
  out.packets = std::move(dem.packets);

  out.index.build(out.packets);
  for (const auto& e : manifest.edits)
    if (e.trackId == spec.id)
      out.boundaries.push_back(alignEditPoint(e, out.packets));

  // 在清单剪辑时间上做随机访问定位，统计平均落点误差。
  double seekSum = 0;
  size_t seekN = 0;
  for (const auto& e : manifest.edits)
    if (e.trackId == spec.id)
      if (auto r = out.index.seek(e.targetUs)) {
        seekSum += std::fabs(usToMs(r->errorUs));
        ++seekN;
      }
  out.seekErrorMs = seekN ? seekSum / double(seekN) : 0.0;
  out.present = true;
  return TimeError::None;
}
} // namespace

TimeError runPipeline(const PipelineConfig& cfg, PipelineReport& rep) {
  Manifest manifest;
  std::map<std::string, Sidecar> cars;
  std::string err;
  if (!loadManifestAndSidecars(cfg.workDir, cfg.manifestPath, manifest, cars,
                               err)) {
    rep.fatalDetail = err;
    rep.fatal = TimeError::IoError;
    return rep.fatal;
  }

  DiagnosticLog diag(cfg.diagPath);
  diag.open();
  for (const auto& spec : manifest.tracks) {
    TrackResult tr;
    tr.id = spec.id;
    tr.type = spec.type;
    auto e = processTrack(spec, cars[spec.id], cfg, manifest, tr, rep.queuePeak);
    if (isFatal(e)) {
      rep.fatal = e;
      rep.firstBadPacket = tr.repair.firstBadPacket;
      rep.fatalDetail =
          "track " + spec.id + " " + toString(e) + " at packet " +
          std::to_string(rep.firstBadPacket);
      diag.append({rep.firstBadPacket, 0, 0, e, rep.fatalDetail});
      diag.close();
      return e;
    }
    // 视频轨解码度量 PSNR 与码率。
    if (spec.type == MediaType::Video) {
      std::vector<bool> corrupt(tr.packets.size(), false);
      for (const auto& p : tr.packets) corrupt[p.index] = p.corrupt;
      std::string mkv = cfg.workDir + "/" + spec.id + ".mkv";
      tr.decode = measureVideo(mkv, corrupt);
    }
    rep.tracks[spec.id] = std::move(tr);
  }
  diag.close();

  // 关键帧索引原子写入并验证完成标记。
  const auto& cam1 = rep.tracks.begin()->second;
  if (!cfg.indexPath.empty()) {
    auto ie = cam1.index.writeAtomic(cfg.indexPath);
    if (ie != TimeError::None || !KeyframeIndex::isComplete(cfg.indexPath)) {
      rep.fatal = TimeError::IoError;
      rep.fatalDetail = "index write incomplete";
      return rep.fatal;
    }
  }
  rep.fatal = TimeError::None;
  return TimeError::None;
}

QualityStats aggregateStats(const PipelineReport& rep, size_t queuePeak,
                            int64_t targetBitrateBps, int64_t durationUs,
                            uint64_t mediaBytes) {
  QualityStats s;
  s.queuePeak = queuePeak;
  s.targetBitrateBps = targetBitrateBps;
  s.durationUs = durationUs;
  uint64_t payloadBytes = 0;
  double psnrWeighted = 0, psnrWeight = 0;
  double driftAbs = 0, driftN = 0, alignMax = 0, seekSum = 0;
  size_t seekN = 0;
  for (const auto& [id, tr] : rep.tracks) {
    s.packetsIn += tr.repair.packetsIn;
    s.packetsRepaired += tr.repair.packetsRepaired + tr.repair.duplicates;
    s.wraps += tr.repair.wraps;
    s.framesCorrupt += tr.decode.framesCorrupt;
    s.framesDecoded += tr.decode.framesDecoded;
    for (const auto& p : tr.packets) payloadBytes += p.payload.size();
    if (tr.type == MediaType::Video && tr.decode.framesDecoded) {
      double w = double(tr.decode.framesDecoded);
      psnrWeighted += tr.decode.psnr.psnrDb() * w;
      psnrWeight += w;
    }
    driftAbs += std::fabs(tr.repair.drift.driftPpm);
    driftN += 1;
    for (const auto& b : tr.boundaries)
      alignMax = std::max(alignMax, std::fabs(usToMs(b.errorUs)));
    if (tr.seekErrorMs > 0) {
      seekSum += tr.seekErrorMs;
      ++seekN;
    }
    s.lowConfidence = s.lowConfidence || tr.repair.drift.lowConfidence;
  }
  s.driftPpm = driftN ? driftAbs / driftN : 0.0;
  s.psnrDb = psnrWeight ? psnrWeighted / psnrWeight : 0.0;
  s.alignmentErrorMs = alignMax;
  s.seekErrorMs = seekN ? seekSum / double(seekN) : 0.0;
  s.payloadBytes = payloadBytes;
  uint64_t bytesForRate = mediaBytes ? mediaBytes : payloadBytes;
  if (durationUs > 0 && bytesForRate > 0)
    s.actualBitrateBps = int64_t(double(bytesForRate) * 8.0 * 1e6 /
                                 double(durationUs));
  s.bitrateErrorPct =
      computeBitrateErrorPct(s.targetBitrateBps, s.actualBitrateBps);
  return s;
}

uint64_t mediaBytesOnDisk(const std::string& dir, const Manifest& m) {
  // 与生成器校准同口径：只统计被校准的主视频轨 cam1，避免多轨汇总偏差。
  uint64_t total = 0;
  std::error_code ec;
  for (const auto& t : m.tracks) {
    if (t.type != MediaType::Video || t.id != "cam1") continue;
    auto sz = std::filesystem::file_size(dir + "/" + t.file, ec);
    if (!ec) total += sz;
  }
  return total;
}
} // namespace media
