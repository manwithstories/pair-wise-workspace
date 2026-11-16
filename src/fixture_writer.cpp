#include "fixture_writer.h"

#include "bit_writer.h"
#include "slice_parser.h"

#include <fstream>

namespace h264 {

void FixtureWriter::writeNal(uint8_t nal_header,
                             const std::vector<uint8_t> &rbsp,
                             bool four_byte) {
  buf_.push_back(0x00);
  buf_.push_back(0x00);
  if (four_byte)
    buf_.push_back(0x00);
  buf_.push_back(0x01);
  buf_.push_back(nal_header);
  auto ebsp = BitWriter::addEmulationBytes(rbsp);
  buf_.insert(buf_.end(), ebsp.begin(), ebsp.end());
}

void FixtureWriter::writeRaw(const std::vector<uint8_t> &bytes) {
  buf_.insert(buf_.end(), bytes.begin(), bytes.end());
}

bool FixtureWriter::saveTo(const std::string &path) const {
  return saveBytes(path, buf_);
}

bool FixtureWriter::saveBytes(const std::string &path,
                              const std::vector<uint8_t> &data) {
  std::ofstream f(path, std::ios::binary | std::ios::trunc);
  if (!f)
    return false;
  f.write(reinterpret_cast<const char *>(data.data()),
          static_cast<std::streamsize>(data.size()));
  return static_cast<bool>(f);
}

namespace {

// 构造 SPS/PPS（与测试夹具同构），interlaced 控制场编码能力
std::vector<uint8_t> buildSps(bool interlaced) {
  BitWriter w;
  w.writeBits(100, 8);
  w.writeBits(0, 8);
  w.writeBits(40, 8);
  w.writeUE(0);
  w.writeUE(1); // chroma 4:2:0
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0); // log2_max_frame_num_minus4
  w.writeUE(0); // poc type 0
  w.writeUE(4);
  w.writeUE(2); // ref frames
  w.writeBit(false);
  w.writeUE(119); // 1920
  w.writeUE(67);  // 1088
  w.writeBit(!interlaced);
  if (interlaced)
    w.writeBit(true);
  w.writeBit(true);
  w.writeBit(true); // crop
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(4); // -> 1080
  w.writeBit(true); // vui
  w.writeBit(false);
  w.writeBit(false);
  w.writeBit(false);
  w.writeBit(false);
  w.writeBit(true); // timing
  w.writeBits(1001, 32);
  w.writeBits(60060, 32);
  w.writeBit(true);
  w.writeRbspTrailingBits();
  return w.bytes();
}

std::vector<uint8_t> buildPps() {
  BitWriter w;
  w.writeUE(0);
  w.writeUE(0);
  w.writeBit(false);
  w.writeBit(false);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeBit(false);
  w.writeBits(0, 2);
  w.writeSE(-3);
  w.writeSE(0);
  w.writeSE(0);
  w.writeBit(true);
  w.writeBit(false);
  w.writeRbspTrailingBits();
  return w.bytes();
}

// 构造一个 slice header RBSP（结构分析所需字段齐全，后接收尾位）
std::vector<uint8_t> buildSlice(uint8_t slice_type, uint32_t first_mb,
                                uint32_t frame_num, bool idr, bool field,
                                bool bottom, bool interlaced_capable) {
  BitWriter w;
  w.writeUE(first_mb);
  w.writeUE(slice_type);
  w.writeUE(0); // pps id
  w.writeBits(frame_num, 4); // log2_max_frame_num_minus4=0 -> 4 位
  if (interlaced_capable) {
    w.writeBit(field);
    if (field)
      w.writeBit(bottom);
  }
  if (idr)
    w.writeUE(0); // idr_pic_id
  w.writeBits(0, 8); // poc_lsb（log2_max_pic_order_cnt_lsb_minus4=4 -> 8 位）
  w.writeRbspTrailingBits();
  return w.bytes();
}

// GOP 内位置 -> 类型：位置 0 为 I，其余按 P/B 节奏
uint8_t frameTypeInGop(uint32_t pos_in_gop) {
  if (pos_in_gop == 0)
    return SLICE_I;
  // 偶数距离给 P，奇数给 B，保证 P/B 都出现且合计为非 I 帧数
  return (pos_in_gop % 2 == 1) ? SLICE_B : SLICE_P;
}

} // namespace

std::vector<uint8_t> generateFixture(const FixtureParams &p) {
  FixtureWriter out;
  const bool interlaced = p.interlaced;

  out.writeNal(0x67, buildSps(interlaced), /*four_byte=*/true);
  out.writeNal(0x68, buildPps());

  uint32_t frame_num = 0;
  for (uint32_t f = 0; f < p.frames; ++f) {
    const bool gop_start = (f % p.gop_period == 0);
    const uint32_t pos = f % p.gop_period;
    const uint8_t stype = frameTypeInGop(pos);
    const bool idr = gop_start; // 每个 GOP 起点用 IDR I 帧
    const uint8_t vcl_header =
        idr ? 0x65 : (stype == SLICE_B ? 0x01 : 0x41);

    if (gop_start) {
      // AUD（primary_pic_type=7，I/P/B 通用）标记随机访问点
      BitWriter aud;
      aud.writeBits(7, 3);
      aud.writeRbspTrailingBits();
      out.writeNal(0x09, aud.bytes());
    }

    auto emitField = [&](bool field, bool bottom) {
      for (uint32_t sl = 0; sl < p.slices_per_frame; ++sl) {
        const uint32_t first_mb = sl * 2700; // 1080p 共 8100 宏块
        auto rbsp = buildSlice(stype, first_mb, frame_num, idr, field,
                               bottom, interlaced);
        out.writeNal(vcl_header, rbsp);
      }
    };

    if (interlaced) {
      emitField(true, false); // 顶场
      emitField(true, true);  // 底场
    } else {
      emitField(false, false);
    }
    frame_num = (frame_num + 1) % 16; // max_frame_num=16
  }

  if (p.append_truncated) {
    // 异常尾部：只有起始码 + NAL 头 + 半个负载，无下一起始码也无收尾
    out.writeRaw({0x00, 0x00, 0x01, 0x41, 0x0A, 0x00});
  }
  return out.bytes();
}

} // namespace h264
