#include "media/generator.hpp"
#include "media/ffmpeg_util.hpp"
#include "media/gen_patterns.hpp"
#include <algorithm>
#include <cmath>
#include <cstdio>
#include <filesystem>
#include <fstream>

namespace media {
namespace fs = std::filesystem;
namespace {

struct TrackArtifacts {
  std::string mediaPath;
  std::string sidecarPath;
  size_t packets = 0;
  uint64_t bytes = 0;
};

// 编码一路视频并同时写侧车。返回空串表示成功。
std::string makeVideo(const GenConfig& cfg, const std::string& id,
                      double ppm, int64_t offsetUs, uint64_t seed,
                      int qscale, TrackArtifacts& art) {
  auto plan = gen::videoPlan(cfg, seed, ppm, offsetUs, cfg.corruptEvery);
  ff::VideoEncodeSpec spec;
  spec.width = cfg.videoWidth;
  spec.height = cfg.videoHeight;
  spec.qscale = qscale;
  size_t ordinal = 0;
  std::ofstream sc(art.sidecarPath, std::ios::trunc);
  sc << "META 1 90000 0 " << plan.size() << '\n';
  auto err = ff::encodeMJpeg(
      art.mediaPath, spec, plan.size(),
      [&](AVFrame* f, size_t i) { gen::fillFrame(f, i, spec.width, spec.height); },
      [&](size_t i) { return plan[i].unwrappedPts; },
      [&](size_t i) {
        const auto& fr = plan[i];
        sc << "PKT " << ordinal << ' ' << id << " video " << fr.pts << ' '
           << fr.duration << ' ' << (fr.keyframe ? 1 : 0) << ' '
           << (fr.corrupt ? 1 : 0) << ' ' << ordinal * 1024 << ' '
           << fr.linearPts << '\n';
        ++ordinal;
        return fr.corrupt;
      });
  if (!err.empty()) return id + ": " + err;
  art.packets = plan.size();
  art.bytes = fs::exists(art.mediaPath) ? fs::file_size(art.mediaPath) : 0;
  return {};
}

std::string makeAudio(const GenConfig& cfg, TrackArtifacts& art) {
  auto plan = gen::audioPlan(cfg, cfg.corruptEvery);
  std::ofstream sc(art.sidecarPath, std::ios::trunc);
  sc << "META 1 " << cfg.sampleRate << ' ' << cfg.sampleRate << ' '
     << plan.size() << '\n';
  size_t ordinal = 0;
  size_t totalSamples = 0;
  for (const auto& a : plan) totalSamples += size_t(a.samples);
  auto err = ff::encodePcmWav(
      art.mediaPath, cfg.sampleRate, 1, totalSamples,
      [&](int16_t* dst, size_t n) {
        const auto& a = plan[ordinal];
        gen::fillAudio(dst, n, ordinal, cfg.sampleRate);
        sc << "PKT " << ordinal << " mic1 audio " << a.pts << ' '
            << a.samples << ' ' << 1 << ' ' << (a.corrupt ? 1 : 0) << ' '
            << ordinal * 2048 << ' ' << a.pts << '\n';
        ++ordinal;
        return;
      });
  if (!err.empty()) return "mic1: " + err;
  art.packets = plan.size();
  art.bytes = fs::exists(art.mediaPath) ? fs::file_size(art.mediaPath) : 0;
  return {};
}

void writeManifest(const GenConfig& cfg, const std::string& path) {
  Manifest m;
  m.tracks = {
      {"cam1", MediaType::Video, "cam1.mkv", {1, 90000}},
      {"cam2", MediaType::Video, "cam2.mkv", {1, 90000}},
      {"mic1", MediaType::Audio, "mic1.wav", {1, cfg.sampleRate}},
  };
  // 剪辑点对齐到 30fps 帧栅格（33.33ms 的整数倍），保证 Exact/Snapped 误差小；
  // 转场放在两条视频之间，重叠区两路来源都保留。
  for (int i = 0; i < cfg.seconds; i += 2) {
    int64_t frameMs = int64_t(std::llround(1000.0 / 30.0)); // 33ms
    int64_t target = (int64_t(i) * 1000 / frameMs) * frameMs;
    m.edits.push_back({m.edits.size(), i % 4 == 0 ? "cam1" : "cam2",
                       target * 1000});
  }
  m.transitions.push_back({0, "cam1", "cam2", 4'000'000, 4'500'000});
  m.transitions.push_back({1, "cam2", "cam1", 8'000'000, 8'500'000});
  std::ofstream f(path);
  f << manifestToText(m);
}
} // namespace

std::string generateSynthetic(const GenConfig& cfg, GenResult& out) {
  std::error_code ec;
  fs::create_directories(cfg.outDir, ec);
  if (ec) return "create out dir: " + ec.message();

  TrackArtifacts cam1{cfg.outDir + "/cam1.mkv", cfg.outDir + "/cam1.sidecar"};
  // 校准：逐档扫描量化等级 2..31，选码率误差最小者；命中 8% 内即提前停止。
  // 报告码率按单轨口径（与校准同口径）。
  double seconds = double(cfg.seconds);
  auto measure = [&](int cand) -> double {
    TrackArtifacts trial{cam1.mediaPath, cam1.sidecarPath};
    if (auto e = makeVideo(cfg, "cam1", cfg.driftPpm, 30'000, 41, cand, trial);
        !e.empty())
      return 1e18;
    int64_t measured = int64_t(double(trial.bytes) * 8.0 / seconds);
    return std::fabs(double(measured - cfg.targetBitrate) /
                     double(cfg.targetBitrate)) * 100.0;
  };
  int bestQ = 8;
  double bestErr = 1e18;
  for (int cand = 2; cand <= 31; ++cand) {
    double errPct = measure(cand);
    if (errPct < bestErr) {
      bestErr = errPct;
      bestQ = cand;
    }
    if (errPct <= 8.0) break; // 码率随 q 单调，越过最优点后无需继续
  }
  int q = bestQ;
  if (auto e = makeVideo(cfg, "cam1", cfg.driftPpm, 30'000, 41, q, cam1);
      !e.empty())
    return e;
  out.chosenQScale = q;

  TrackArtifacts cam2{cfg.outDir + "/cam2.mkv", cfg.outDir + "/cam2.sidecar"};
  if (auto e = makeVideo(cfg, "cam2", -cfg.driftPpm * 0.5, -20'000, 73, q,
                         cam2);
      !e.empty())
    return e;
  TrackArtifacts mic1{cfg.outDir + "/mic1.wav", cfg.outDir + "/mic1.sidecar"};
  if (auto e = makeAudio(cfg, mic1); !e.empty()) return e;

  out.manifestPath = cfg.outDir + "/manifest.txt";
  writeManifest(cfg, out.manifestPath);
  out.files = {cam1.mediaPath, cam2.mediaPath, mic1.mediaPath};
  out.sidecars = {cam1.sidecarPath, cam2.sidecarPath, mic1.sidecarPath};
  return {};
}
} // namespace media
