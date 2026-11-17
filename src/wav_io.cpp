// wav_io.cpp — RIFF/WAVE 解析（fmt + data 块）、PCM 编解码、原子写
#include "wav_io.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <limits>
#include <vector>

#if defined(_WIN32)
#include <windows.h>
#else
#include <cerrno>
#endif
#include <filesystem>

namespace popfix {

namespace {

std::uint32_t rd_u32(const unsigned char* p) {
  return static_cast<std::uint32_t>(p[0]) |
         (static_cast<std::uint32_t>(p[1]) << 8) |
         (static_cast<std::uint32_t>(p[2]) << 16) |
         (static_cast<std::uint32_t>(p[3]) << 24);
}
std::uint16_t rd_u16(const unsigned char* p) {
  return static_cast<std::uint16_t>(p[0] |
                                    (static_cast<std::uint16_t>(p[1]) << 8));
}
void wr_u32(std::vector<unsigned char>& b, std::uint32_t v) {
  for (int i = 0; i < 4; ++i) b.push_back(static_cast<unsigned char>(v >> (8 * i)));
}
void wr_u16(std::vector<unsigned char>& b, std::uint16_t v) {
  b.push_back(static_cast<unsigned char>(v & 0xFF));
  b.push_back(static_cast<unsigned char>(v >> 8));
}

double decode_signed(std::int32_t v, int bits) {
  const double scale = std::ldexp(1.0, bits - 1);  // 2^(bits-1)
  return static_cast<double>(v) / scale;
}
std::int32_t encode_signed(double s, int bits) {
  const double scale = std::ldexp(1.0, bits - 1);
  double scaled = std::round(s * scale);
  const double lo = -scale;
  const double hi = scale - 1.0;
  if (scaled < lo) scaled = lo;
  if (scaled > hi) scaled = hi;
  return static_cast<std::int32_t>(scaled);
}

std::string fourcc(const unsigned char* p) {
  return std::string(reinterpret_cast<const char*>(p), 4);
}

}  // namespace

void ensure_parent_dir(const std::string& path) {
  const std::filesystem::path p(path);
  const std::filesystem::path parent = p.parent_path();
  if (parent.empty()) return;
  std::error_code ec;
  std::filesystem::create_directories(parent, ec);
  if (ec) {
    throw WavIOError("无法创建输出目录 " + parent.string() + ": " +
                         ec.message(),
                     true);
  }
}

void validate_output_path(const std::string& path) {
  if (path.empty()) {
    throw WavIOError("输出路径为空", true);
  }
  std::string component;
  auto flush = [&] {
    if (component == "..") {
      throw WavIOError("拒绝包含路径穿越 (..) 的输出路径: " + path, true);
    }
    component.clear();
  };
  for (char c : path) {
    if (c == '/' || c == '\\') {
      flush();
    } else {
      component.push_back(c);
    }
  }
  flush();
}

AudioBuffer read_wav(const std::string& path, WavReadInfo& info) {
  std::ifstream f(path, std::ios::binary);
  if (!f) throw WavIOError("无法打开输入文件: " + path, false);

  std::vector<unsigned char> bytes((std::istreambuf_iterator<char>(f)),
                                   std::istreambuf_iterator<char>());
  if (bytes.size() < 44) {
    throw WavIOError("文件过小，不是合法 WAV: " + path, false);
  }
  if (fourcc(bytes.data()) != "RIFF" || fourcc(bytes.data() + 8) != "WAVE") {
    throw WavIOError("缺少 RIFF/WAVE 标识: " + path, false);
  }
  info.riff_size = rd_u32(bytes.data() + 4);

  std::uint16_t fmt_tag = 0, channels = 0, bits = 0;
  std::uint32_t rate = 0;
  bool have_fmt = false;
  std::size_t data_pos = 0;
  std::uint32_t data_size = 0;
  bool have_data = false;

  std::size_t pos = 12;
  while (pos + 8 <= bytes.size()) {
    const std::string id = fourcc(bytes.data() + pos);
    std::uint32_t csize = rd_u32(bytes.data() + pos + 4);
    std::size_t body = pos + 8;
    if (body > bytes.size() || csize > bytes.size() - body) {
      throw WavIOError("块长度越界: " + id, false);
    }
    if (id == "fmt ") {
      if (csize < 16) throw WavIOError("fmt 块过短", false);
      fmt_tag = rd_u16(bytes.data() + body);
      channels = rd_u16(bytes.data() + body + 2);
      rate = rd_u32(bytes.data() + body + 4);
      bits = rd_u16(bytes.data() + body + 14);
      have_fmt = true;
    } else if (id == "data") {
      data_pos = body;
      data_size = csize;
      have_data = true;
    }
    // 其它块（LIST/fact 等）按规范跳过；块按字对齐填充。
    pos = body + csize + (csize & 1U);
  }

  if (!have_fmt) throw WavIOError("缺少 fmt 块", false);
  if (!have_data) throw WavIOError("缺少 data 块", false);
  if (fmt_tag != 1) {
    throw WavIOError("仅支持 PCM(format_tag=1)，拒绝压缩格式: " +
                         std::to_string(fmt_tag),
                     false);
  }
  if (channels != 1 && channels != 2) {
    throw WavIOError("仅支持单声道或双声道，实际声道数: " +
                         std::to_string(channels),
                     false);
  }
  if (bits != 16 && bits != 24 && bits != 32) {
    throw WavIOError("未知位深（仅支持 16/24/32）: " + std::to_string(bits),
                     false);
  }
  if (rate == 0) throw WavIOError("非法采样率 0", false);

  const std::uint32_t bytes_per_sample = bits / 8;
  const std::uint32_t frame_bytes = bytes_per_sample * channels;
  if (frame_bytes == 0) throw WavIOError("块长度非法", false);

  const std::size_t available = bytes.size() - data_pos;
  if (data_size > available) {
    info.truncated = true;
    throw WavIOError("数据块越界/文件截断: 声明 " +
                         std::to_string(data_size) + " 字节，实际 " +
                         std::to_string(available),
                     false);
  }
  const std::size_t frames = data_size / frame_bytes;
  if (frames == 0) throw WavIOError("空音频：data 块无完整帧", false);
  if (frames > kMaxFrames) {
    throw WavIOError("帧数超过安全上限 " + std::to_string(kMaxFrames) +
                         "，拒绝分配内存",
                     false);
  }

  AudioBuffer audio;
  audio.sample_rate = rate;
  audio.channels = channels;
  audio.bits_per_sample = bits;
  audio.data.resize(frames * static_cast<std::size_t>(channels));
  info.data_size = data_size;

  for (std::size_t i = 0; i < audio.data.size(); ++i) {
    const std::size_t off = data_pos + i * bytes_per_sample;
    std::int32_t v = 0;
    if (bits == 16) {
      v = static_cast<std::int16_t>(rd_u16(bytes.data() + off));
    } else if (bits == 24) {
      std::int32_t raw = static_cast<std::int32_t>(
          bytes[off] | (bytes[off + 1] << 8) | (bytes[off + 2] << 16));
      if (raw & 0x800000) raw |= ~0xFFFFFF;  // 符号扩展
      v = raw;
    } else {
      v = static_cast<std::int32_t>(rd_u32(bytes.data() + off));
    }
    audio.data[i] = decode_signed(v, bits);
  }
  return audio;
}

void write_wav_atomic(const std::string& path, const AudioBuffer& audio) {
  validate_output_path(path);
  if (audio.channels != 1 && audio.channels != 2) {
    throw WavIOError("仅支持单声道或双声道写出", true);
  }
  const int bits = audio.bits_per_sample;
  const std::uint32_t bps = bits / 8;
  const std::uint32_t frame_bytes = bps * audio.channels;
  const std::uint32_t data_size =
      static_cast<std::uint32_t>(audio.frames() * frame_bytes);
  const std::uint32_t riff_size = 36 + data_size;

  std::vector<unsigned char> out;
  out.reserve(44 + data_size);
  out.insert(out.end(), {'R', 'I', 'F', 'F'});
  wr_u32(out, riff_size);
  out.insert(out.end(), {'W', 'A', 'V', 'E'});
  out.insert(out.end(), {'f', 'm', 't', ' '});
  wr_u32(out, 16);
  wr_u16(out, 1);                              // PCM
  wr_u16(out, audio.channels);
  wr_u32(out, audio.sample_rate);
  wr_u32(out, audio.sample_rate * frame_bytes);  // 字节率
  wr_u16(out, frame_bytes);                      // 块对齐
  wr_u16(out, static_cast<std::uint16_t>(bits));
  out.insert(out.end(), {'d', 'a', 't', 'a'});
  wr_u32(out, data_size);

  for (double s : audio.data) {
    std::int32_t v = encode_signed(s, bits);
    if (bits == 16) {
      wr_u16(out, static_cast<std::uint16_t>(static_cast<std::int16_t>(v)));
    } else if (bits == 24) {
      std::uint32_t u = static_cast<std::uint32_t>(v);
      out.push_back(static_cast<unsigned char>(u & 0xFF));
      out.push_back(static_cast<unsigned char>((u >> 8) & 0xFF));
      out.push_back(static_cast<unsigned char>((u >> 16) & 0xFF));
    } else {
      wr_u32(out, static_cast<std::uint32_t>(v));
    }
  }

  const std::string tmp = path + ".tmp";
  ensure_parent_dir(path);
  std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
  if (!f) {
    throw WavIOError("无法创建临时文件: " + tmp, true);
  }
  f.write(reinterpret_cast<const char*>(out.data()),
          static_cast<std::streamsize>(out.size()));
  if (!f) {
    f.close();
    std::remove(tmp.c_str());
    throw WavIOError("写临时文件失败: " + tmp, true);
  }
  f.close();

#if defined(_WIN32)
  if (!MoveFileExA(tmp.c_str(), path.c_str(), MOVEFILE_REPLACE_EXISTING)) {
    std::remove(tmp.c_str());
    throw WavIOError("原子替换失败: " + path, true);
  }
#else
  if (std::rename(tmp.c_str(), path.c_str()) != 0) {
    std::remove(tmp.c_str());
    throw WavIOError("原子替换失败: " + path + " (errno=" +
                         std::to_string(errno) + ")",
                     true);
  }
#endif
}

}  // namespace popfix
