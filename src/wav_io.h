// wav_io.h — WAV PCM 读写：16/24/32 位整数 PCM，校验与原子替换写入
//
// 仅支持本地文件与内存处理；拒绝压缩格式、空文件、未知位深与数据块越界。
// 样本在内存中以 double 归一化到 [-1, 1]，峰值计算天然避免整数溢出。
#pragma once

#include <cstdint>
#include <stdexcept>
#include <string>

#include "sample_model.h"

namespace popfix {

// 所有 WAV 读写错误均以该异常抛出，由 main 映射为退出码 2（输入错误）
// 或 5（写入失败）。
class WavIOError : public std::runtime_error {
 public:
  WavIOError(const std::string& msg, bool write_failure)
      : std::runtime_error(msg), write_failure_(write_failure) {}
  bool write_failure() const { return write_failure_; }

 private:
  bool write_failure_;
};

struct WavReadInfo {
  bool truncated = false;  // 数据块声明长度超过文件实际可用字节
  std::uint32_t riff_size = 0;
  std::uint32_t data_size = 0;
};

// 读取并严格校验 WAV。失败抛 WavIOError；truncated 时同样抛错，
// 但错误信息包含 "truncated" 以便测试与上层区分。
AudioBuffer read_wav(const std::string& path, WavReadInfo& info);

// 以缓冲当前位深写出 WAV。采用 "同目录临时文件 + rename 原子替换"，
// 失败时清理临时文件并抛 write_failure=true 的 WavIOError。
void write_wav_atomic(const std::string& path, const AudioBuffer& audio);

// 拒绝路径穿越：规范化词法组件后不得包含 ".."，不得为空。
// 非法时抛 WavIOError(write_failure=true)。
void validate_output_path(const std::string& path);

// 确保输出文件的父目录存在（幂等）；失败抛 WavIOError(写失败)。
void ensure_parent_dir(const std::string& path);

// 帧数硬上限：防止畸形头部声称超大帧数导致内存失控。
// 48kHz 双声道 10 分钟约 2.88e7 帧，上限取 1e8 足够宽松。
constexpr std::size_t kMaxFrames = 100'000'000;

}  // namespace popfix
