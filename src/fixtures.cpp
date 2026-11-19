#include "fixtures.hpp"

#include <fstream>
#include <sstream>

#include "checkpoint.hpp"
#include "subtitle.hpp"

namespace mb {

namespace fs = std::filesystem;

namespace {
// 合格 SRT：起点全部落在 25fps 帧网格（40ms 整数倍），对齐误差为 0。
std::string good_srt() {
  std::ostringstream o;
  o << "1\n00:00:00,000 --> 00:00:00,480\n开场字幕\n\n"
    << "2\n00:00:00,520 --> 00:00:01,000\n节目标题\n\n"
    << "3\n00:00:01,040 --> 00:00:01,520\n第二段说明\n\n"
    << "4\n00:00:01,560 --> 00:00:01,960\n结束字幕\n";
  return o.str();
}

// 合格 VTT：点号时间戳，可选 cue id。
std::string good_vtt() {
  std::ostringstream o;
  o << "WEBVTT\n\n"
    << "id-1\n00:00:00.200 --> 00:00:00.600\nlower third\n\n"
    << "00:00:00.800 --> 00:00:01.200\nbanner\n";
  return o.str();
}

// 损坏 SRT：错误时间（逆序）、截断时间轴、与前条重叠，且含 20ms 错位。
std::string damaged_srt() {
  std::ostringstream o;
  o << "1\n00:00:00,020 --> 00:00:00,500\n错位字幕\n\n"          // 20ms 错位
    << "2\n00:00:01,000 --> 00:00:00,500\n逆序时间坏记录\n\n"  // 非法逆序
    << "3\n00:00:00,400 --> 00:00:00,900\n与第一条重叠\n\n"    // 重叠
    << "4\n00:00:01,200 -->\n截断的时间轴\n\n"                  // 截断
    << "5\n00:00:01,400 --> 00:00:01,800\n恢复后的有效字幕\n";
  return o.str();
}

bool write_file(const fs::path& p, const std::string& data, std::string* fatal) {
  std::ofstream f(p, std::ios::binary | std::ios::trunc);
  if (!f) { if (fatal) *fatal = "cannot write fixture: " + p.string();
    return false; }
  f.write(data.data(), static_cast<std::streamsize>(data.size()));
  return bool(f);
}
}  // namespace

bool generate_fixtures(const FixtureConfig& cfg, FixtureSet* out,
                       ErrList* errs, std::string* fatal) {
  fs::path root = cfg.work_dir / "fixtures";
  if (!path_within(cfg.work_dir, root)) {
    if (fatal) *fatal = "fixture dir escapes work directory";
    return false;
  }
  std::string ioerr;
  if (!ensure_dir(root, &ioerr)) { if (fatal) *fatal = ioerr; return false; }

  FixtureSet fx;
  fx.root = root;
  fx.srt_path = (root / (cfg.damaged ? "bad.srt" : "good.srt")).string();
  fx.vtt_path = (root / (cfg.damaged ? "bad.vtt" : "good.vtt")).string();
  fx.media_path = (root / (cfg.damaged ? "damaged.mkv" : "program.mkv")).string();

  if (!write_file(fs::path(fx.srt_path),
                  cfg.damaged ? damaged_srt() : good_srt(), fatal))
    return false;
  if (!write_file(fs::path(fx.vtt_path), good_vtt(), fatal)) return false;

  MediaSpec spec;
  spec.path = fx.media_path;
  spec.fps = cfg.fps;
  spec.duration_ms = cfg.duration_ms;
  spec.video_bitrate = cfg.media_bitrate;
  spec.inject_corruption = cfg.damaged;  // 仅损坏夹具注入字节破坏
  spec.corrupt_seed = cfg.damaged ? 98765 : 12345;
  std::string merr;
  if (!make_test_media(spec, &merr)) {
    if (fatal) *fatal = "media fixture failed: " + merr;
    return false;
  }
  fx.media_spec = spec;

  // 输入清单：记录夹具路径、帧率、时长、损坏标志，作为检查点摘要来源。
  std::ostringstream mf;
  mf << "media=" << fx.media_path << '\n'
     << "srt=" << fx.srt_path << '\n'
     << "vtt=" << fx.vtt_path << '\n'
     << "fps=" << cfg.fps.num << '/' << cfg.fps.den << '\n'
     << "duration_ms=" << cfg.duration_ms << '\n'
     << "damaged=" << (cfg.damaged ? 1 : 0) << '\n'
     << "seed=" << spec.corrupt_seed << '\n';
  fx.manifest = mf.str();
  std::string manifest_path = (root / "input.manifest").string();
  if (!write_file(fs::path(manifest_path), fx.manifest, fatal)) return false;

  // 解析一遍字幕，把夹具自带问题作为可恢复错误透出（不致命）。
  std::ifstream sf(fx.srt_path, std::ios::binary);
  std::string srt((std::istreambuf_iterator<char>(sf)), {});
  SubtitleDoc doc = parse_subtitles(srt, SubFormat::kSrt);
  if (errs)
    for (const auto& e : doc.errors) errs->push_back(e);
  *out = std::move(fx);
  return true;
}

std::string fixture_manifest_digest(const FixtureSet& fx) {
  return CheckpointStore::compute_digest(fx.manifest);
}

}  // namespace mb
