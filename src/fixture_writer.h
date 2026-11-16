#pragma once
// 夹具流写入器：把构造好的 RBSP 加防竞争字节并以 Annex B 起始码落盘，
// 供 generate 子命令与测试共用。输出确定性内容（无随机种子、无时间戳）。
#include <cstdint>
#include <iosfwd>
#include <vector>

namespace h264 {

class FixtureWriter {
public:
  // 写入一个完整 NAL：起始码 + NAL 头 + 防竞争处理后的 RBSP
  void writeNal(uint8_t nal_header, const std::vector<uint8_t> &rbsp,
                bool four_byte = false);

  // 追加原始字节（用于构造异常截断尾部）
  void writeRaw(const std::vector<uint8_t> &bytes);

  bool saveTo(const std::string &path) const;
  const std::vector<uint8_t> &bytes() const { return buf_; }
  size_t size() const { return buf_.size(); }

  // 直接把任意缓冲落盘（供 generateFixture 的返回值使用）
  static bool saveBytes(const std::string &path,
                        const std::vector<uint8_t> &data);

private:
  std::vector<uint8_t> buf_;
};

// 生成完整确定性样例码流的参数
struct FixtureParams {
  uint32_t frames = 24;
  uint32_t slices_per_frame = 3;
  bool interlaced = false;
  uint32_t gop_period = 8;   // 24 帧 -> IDR 落在 0/8/16，共 3 个图像组
  bool append_truncated = true; // 末尾追加异常截断 NAL
};

// 生成样例到返回的缓冲（同时可经 FixtureWriter 落盘）
std::vector<uint8_t> generateFixture(const FixtureParams &params);

} // namespace h264
