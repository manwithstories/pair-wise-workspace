#include "media/fixture.hpp"

#include <cmath>
#include <cstdio>
#include <filesystem>
#include <fstream>

namespace media::fixture {
namespace fs = std::filesystem;

namespace {

void put16(std::ofstream& f, uint16_t v) {
  char b[2] = {static_cast<char>(v & 0xFF), static_cast<char>(v >> 8)};
  f.write(b, 2);
}
void put32(std::ofstream& f, uint32_t v) {
  char b[4] = {static_cast<char>(v), static_cast<char>(v >> 8),
               static_cast<char>(v >> 16), static_cast<char>(v >> 24)};
  f.write(b, 4);
}

// 确定性正弦：用整数样本序号累加，不使用浮点相位累计漂移。
// 返回 16-bit 有符号样本（-32768..32767），避免强转溢出 UB。
int16_t toneSample(uint64_t frame, uint32_t rate, double freq, double amp) {
  double phase = 2.0 * M_PI * freq * static_cast<double>(frame) / rate;
  double v = std::sin(phase) * amp * 32767.0;
  if (v >= 32767.0) return 32767;
  if (v <= -32768.0) return -32768;
  return static_cast<int16_t>(v);
}

// 16-bit 满幅样本提升到内部 32-bit 满幅刻度（左移 16 位可精确还原）。
int32_t fullScale16(bool negative) {
  return static_cast<int32_t>(negative ? -32768 : 32767) << 16;
}

}  // namespace

WavWriter::WavWriter(std::string path, uint16_t channels,
                     uint32_t sampleRate, uint16_t bitsPerSample)
    : path_(std::move(path)),
      channels_(channels),
      sampleRate_(sampleRate),
      bits_(bitsPerSample) {
  fs::create_directories(fs::path(path_).parent_path());
}

void WavWriter::appendFrames(const std::vector<int32_t>& interleaved) {
  if (dataBytes_ == 0) {
    out_.open(path_, std::ios::binary | std::ios::trunc);
    out_.write("RIFF", 4);
    put32(out_, 0);  // 占位，close 回填
    out_.write("WAVEfmt ", 8);
    put32(out_, 16);
    put16(out_, 1);
    put16(out_, channels_);
    put32(out_, sampleRate_);
    uint16_t blockAlign = channels_ * (bits_ / 8);
    put32(out_, sampleRate_ * blockAlign);
    put16(out_, blockAlign);
    put16(out_, bits_);
    out_.write("data", 4);
    put32(out_, 0);
  }
  for (int32_t s : interleaved) {
    int16_t v = static_cast<int16_t>(s >> 16);  // 32-bit 满幅刻度 -> 16-bit
    put16(out_, static_cast<uint16_t>(v));
    dataBytes_ += 2;
  }
}

void WavWriter::close() {
  if (closed_) return;
  closed_ = true;
  if (!out_.is_open()) return;
  out_.seekp(4);
  put32(out_, 36 + dataBytes_);
  out_.seekp(40);
  put32(out_, dataBytes_);
  out_.close();
}

FixtureSet generateAll(const std::string& dir, uint32_t rate) {
  fs::create_directories(dir);
  FixtureSet set;
  set.stereoPath = (fs::path(dir) / "stereo.wav").string();
  set.truncatedPath = (fs::path(dir) / "truncated.wav").string();
  set.nonIntegerPath = (fs::path(dir) / "non_integer.wav").string();
  set.duplicatePath = (fs::path(dir) / "duplicate.wav").string();

  const uint32_t seconds = 10;
  const uint64_t total = static_cast<uint64_t>(rate) * seconds;

  // 1) stereo.wav：0-2s 440Hz、2-4s 静音、4-6s 削波、6-8s 880Hz、8-10s 重复 440Hz。
  {
    WavWriter w(set.stereoPath, 2, rate);
    std::vector<int32_t> buf;
    buf.reserve(rate * 2 * 2);  // 1 秒立体声样本数
    for (uint64_t i = 0; i < total; ++i) {
      int32_t l = 0, r = 0;
      if (i < 2 * rate) {
        l = static_cast<int32_t>(toneSample(i, rate, 440.0, 0.5)) << 16;
        r = static_cast<int32_t>(toneSample(i, rate, 440.0, 0.5)) << 16;
      } else if (i < 4 * rate) {
        l = r = 0;
      } else if (i < 6 * rate) {
        // 满幅削波：16-bit 极值左移 16 位后精确命中 INT32_MIN/MAX。
        l = fullScale16(i & 1);
        r = l;
      } else if (i < 8 * rate) {
        l = static_cast<int32_t>(toneSample(i, rate, 880.0, 0.4)) << 16;
        r = static_cast<int32_t>(toneSample(i, rate, 660.0, 0.4)) << 16;
      } else {
        // 与开头同源，制造可判重的重复内容。
        l = static_cast<int32_t>(toneSample(i - 8 * rate, rate, 440.0, 0.5))
            << 16;
        r = static_cast<int32_t>(toneSample(i - 8 * rate, rate, 440.0, 0.5))
            << 16;
      }
      buf.push_back(l);
      buf.push_back(r);
      if (buf.size() >= rate * 2 * 2) {
        w.appendFrames(buf);
        buf.clear();
      }
    }
    if (!buf.empty()) w.appendFrames(buf);
  }

  // 2) truncated.wav：正常 4s 素材后直接截断文件（data 声明长度大于实际）。
  {
    WavWriter w(set.truncatedPath, 2, rate);
    std::vector<int32_t> buf;
    for (uint64_t i = 0; i < static_cast<uint64_t>(rate) * 4; ++i) {
      int32_t v = static_cast<int32_t>(toneSample(i, rate, 330.0, 0.5)) << 16;
      buf.push_back(v);
      buf.push_back(v);
    }
    w.appendFrames(buf);
    w.close();
    // 把 data chunk 长度改大但不追加字节，制造帧中途截断。
    std::FILE* fp = std::fopen(set.truncatedPath.c_str(), "r+b");
    if (fp) {
      std::fseek(fp, 40, SEEK_SET);
      uint32_t inflated = 4 * rate * 4 + 37;  // 多出 37 字节，非整帧
      unsigned char b[4] = {static_cast<unsigned char>(inflated),
                            static_cast<unsigned char>(inflated >> 8),
                            static_cast<unsigned char>(inflated >> 16),
                            static_cast<unsigned char>(inflated >> 24)};
      std::fwrite(b, 1, 4, fp);
      std::fclose(fp);
    }
  }

  // 3) non_integer.wav：完整帧后追加 3 个孤立字节。
  {
    WavWriter w(set.nonIntegerPath, 2, rate);
    std::vector<int32_t> buf;
    for (uint64_t i = 0; i < static_cast<uint64_t>(rate) * 2; ++i) {
      int32_t v = static_cast<int32_t>(toneSample(i, rate, 220.0, 0.5)) << 16;
      buf.push_back(v);
      buf.push_back(v);
    }
    w.appendFrames(buf);
    w.close();
    std::ofstream f(set.nonIntegerPath, std::ios::binary | std::ios::app);
    f.write("\x01\x02\x03", 3);
  }

  // 4) duplicate.wav：两段完全相同的 2s 内容相邻排列。
  {
    WavWriter w(set.duplicatePath, 1, rate);
    std::vector<int32_t> block;
    for (uint64_t i = 0; i < 2 * rate; ++i)
      block.push_back(
          static_cast<int32_t>(toneSample(i, rate, 523.0, 0.6)) << 16);
    w.appendFrames(block);
    w.appendFrames(block);  // 逐样本相同 -> 指纹判重
  }

  return set;
}

}  // namespace media::fixture
