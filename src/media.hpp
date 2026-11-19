// FFmpeg 媒体封装：合成测试素材、解封装/解码统计、按目标码率再封装。
// 全部基于 libavformat/libavcodec/libswresample，离屏运行，无任何窗口依赖。
#pragma once
#include <cstdint>
#include <string>
#include <vector>

#include "timebase.hpp"
#include "util.hpp"

namespace mb {

struct MediaSpec {
  std::string path;
  int width = 320;
  int height = 240;
  Ratio fps{25, 1};
  int64_t duration_ms = 2000;
  int64_t video_bitrate = 1'500'000;
  int audio_channels = 1;
  int audio_sample_rate = 48000;
  int64_t audio_bitrate = 64'000;
  bool include_audio = true;
  bool inject_corruption = false;  // 写出后破坏中段字节，模拟损坏媒体
  int64_t corrupt_seed = 12345;
};

struct MediaStats {
  int64_t video_frames = 0;
  int64_t audio_frames = 0;
  int64_t duration_ms = 0;
  double actual_video_bitrate_bps = 0.0;
  double audio_peak_dbfs = -99.0;
  double video_psnr_db = 0.0;
  int decode_errors = 0;
  int missing_tracks = 0;
};

// 合成确定性图案/正弦音的素材（H.264 + AAC，MKV），可选注入字节损坏。
bool make_test_media(const MediaSpec& spec, std::string* err);

// 解封装并完整解码，统计帧数、时长、相对合成参考的 PSNR、音频峰值 dBFS。
// tolerate_errors=true 时解码错误计入 stats 而非失败（错误继续策略）。
bool probe_media(const std::string& path, const MediaSpec& spec,
                 MediaStats* stats, bool tolerate_errors, ErrList* errs);

struct TranscodeResult {
  MediaStats stats;
  uint64_t output_bytes = 0;
  // 失败区间（主刻度 start,end），用于检查点标记；空表示全程成功。
  std::vector<std::pair<int64_t, int64_t>> failed_ranges;
  bool ok = false;
};

// 再封装：解码输入并按目标视频码率/目标封装重新编码（CBR 风格）。
// 缺少音轨/解码错误时保留已处理部分并记录失败区间。
bool transcode_media(const std::string& in_path, const std::string& out_path,
                     const MediaSpec& spec, int64_t target_video_bitrate,
                     const std::string& container, TranscodeResult* out,
                     ErrList* errs);

// 在文件中段翻转若干字节，制造确定性损坏夹具。
bool inject_file_corruption(const std::string& path, int64_t seed,
                            std::string* err);

inline constexpr const char* kMediaDecode = "MEDIA_DECODE_ERROR";
inline constexpr const char* kMediaMissing = "MEDIA_TRACK_MISSING";
inline constexpr const char* kMediaWrite = "MEDIA_WRITE_FAILED";
inline constexpr const char* kMediaOpen = "MEDIA_OPEN_FAILED";

}  // namespace mb
