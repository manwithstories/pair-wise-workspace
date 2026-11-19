// 合成素材生成与 MJPEG 解码质量的端到端测试。
#include "test_helpers.hpp"
#include "media/decoder.hpp"
#include "media/demuxer.hpp"
#include "media/gen_patterns.hpp"
#include "media/generator.hpp"

TEST_CASE("合成视频时序：漂移/VFR/33 位回绕", "[generator]") {
  GenConfig cfg;
  cfg.seconds = 12;
  cfg.wrapPts = true;
  cfg.vfr = true;
  auto plan = media::gen::videoPlan(cfg, 41, 180.0, 30'000, 97);
  REQUIRE(plan.size() == 360);
  size_t wraps = 0;
  for (const auto& f : plan)
    if (f.wrapped) ++wraps;
  CHECK(wraps == 1);
  CHECK(plan.front().pts > int64_t(PTS33_MASK) - 90000 * 13);
  bool varied = false;
  for (size_t i = 2; i < plan.size(); ++i)
    if (plan[i].duration != plan[i - 1].duration) varied = true;
  CHECK(varied);
  size_t corrupt = 0;
  for (const auto& f : plan) if (f.corrupt) ++corrupt;
  CHECK(corrupt >= 2);

  GenConfig flat = cfg;
  flat.vfr = false;
  auto cfr = media::gen::videoPlan(flat, 41, 0.0, 0, 0);
  for (size_t i = 1; i < cfr.size(); ++i)
    CHECK(cfr[i].duration == cfr[1].duration);

  auto audio = media::gen::audioPlan(cfg, 97);
  CHECK(audio.size() == size_t(12 * 24000 / 1024) + 1);
  CHECK(audio.front().samples == 1024);
}

TEST_CASE("端到端合成：媒体/侧车/清单生成且确定性可复现", "[generator]") {
  auto base = std::filesystem::temp_directory_path() /
              ("md_gen_" + std::to_string(::getpid()));
  GenConfig cfg;
  cfg.outDir = (base / "run1").string();
  cfg.seconds = 4;
  cfg.driftPpm = 180;
  cfg.wrapPts = true;
  cfg.vfr = true;
  cfg.corruptEvery = 37;
  GenResult r1;
  REQUIRE(generateSynthetic(cfg, r1).empty());
  for (const auto& f : r1.files) CHECK(std::filesystem::exists(f));
  for (const auto& f : r1.sidecars) CHECK(std::filesystem::exists(f));
  CHECK(std::filesystem::exists(r1.manifestPath));
  CHECK(std::filesystem::file_size(r1.files[0]) > 1000);
  CHECK(std::filesystem::file_size(r1.files[2]) > 1000);

  Sidecar sc;
  std::string err;
  REQUIRE(loadSidecar(r1.sidecars[0], sc, err));
  CHECK(sc.entries.size() == 4u * 30u);
  size_t keyframes = 0, corrupt = 0;
  for (const auto& e : sc.entries) {
    if (e.keyframe) ++keyframes;
    if (e.corrupt) ++corrupt;
  }
  CHECK(keyframes >= 8);
  CHECK(corrupt >= 1);

  GenConfig cfg2 = cfg;
  cfg2.outDir = (base / "run2").string();
  GenResult r2;
  REQUIRE(generateSynthetic(cfg2, r2).empty());
  auto readAll = [](const std::string& p) {
    std::ifstream f(p, std::ios::binary);
    return std::string(std::istreambuf_iterator<char>(f),
                       std::istreambuf_iterator<char>());
  };
  CHECK(readAll(r1.sidecars[0]) == readAll(r2.sidecars[0]));

  Manifest m;
  std::ifstream mf(r1.manifestPath);
  std::string text((std::istreambuf_iterator<char>(mf)),
                   std::istreambuf_iterator<char>());
  REQUIRE_FALSE(parseManifest(text, m));
  CHECK(m.tracks.size() == 3);
  CHECK_FALSE(m.transitions.empty());
  std::filesystem::remove_all(base);
}

TEST_CASE("MJPEG 解码与 PSNR 质量度量", "[decoder]") {
  auto base = std::filesystem::temp_directory_path() /
              ("md_dec_" + std::to_string(::getpid()));
  GenConfig cfg;
  cfg.outDir = (base / "g").string();
  cfg.seconds = 3;
  cfg.wrapPts = true;
  cfg.vfr = true;
  cfg.corruptEvery = 11;
  GenResult r;
  REQUIRE(generateSynthetic(cfg, r).empty());

  Sidecar sc;
  std::string err;
  REQUIRE(loadSidecar(r.sidecars[0], sc, err));
  std::vector<bool> corrupt(sc.entries.size(), false);
  for (const auto& e : sc.entries)
    if (e.corrupt) corrupt[e.ordinal] = true;

  auto out = measureVideo(r.files[0], corrupt);
  CHECK(out.framesDecoded > 0);
  CHECK(out.framesCorrupt >= 1);
  CHECK(out.psnr.psnrDb() > 25.0);
  CHECK(out.psnr.psnrDb() < 45.0);

  GenConfig clean = cfg;
  clean.corruptEvery = 0;
  clean.outDir = (base / "clean").string();
  GenResult rc2;
  REQUIRE(generateSynthetic(clean, rc2).empty());
  std::vector<bool> none(90, false);
  auto good = measureVideo(rc2.files[0], none);
  CHECK(good.psnr.psnrDb() >= out.psnr.psnrDb());
  std::filesystem::remove_all(base);
}
