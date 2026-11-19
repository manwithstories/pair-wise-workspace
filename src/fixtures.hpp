// 夹具生成：全部样例数据由程序确定性生成，不引用任何外部文件。
// 产出 good（合格）与 damaged（截断字幕/错误时间码/重叠/损坏媒体）两套。
#pragma once
#include <filesystem>
#include <string>

#include "media.hpp"
#include "util.hpp"

namespace mb {

struct FixtureSet {
  std::filesystem::path root;
  std::string srt_path;
  std::string vtt_path;
  std::string media_path;
  std::string manifest;   // 输入清单（其 SHA-256 作为检查点输入摘要）
  MediaSpec media_spec;
};

struct FixtureConfig {
  std::filesystem::path work_dir;
  bool damaged = false;
  int64_t media_bitrate = 1'500'000;
  Ratio fps{25, 1};
  int64_t duration_ms = 2000;
};

// 在 work_dir/fixtures 下生成夹具；拒绝越出工作目录。
bool generate_fixtures(const FixtureConfig& cfg, FixtureSet* out,
                       ErrList* errs, std::string* fatal);

// 由夹具内容构造确定性输入清单并返回摘要（输入变化 -> 检查点拒绝复用）。
std::string fixture_manifest_digest(const FixtureSet& fx);

}  // namespace mb
