#pragma once
// 测试/夹具共用的 H.264 语法构造助手：生成结构合法的参数集与条带头。
#include <cstdint>
#include <vector>

#include "bit_writer.h"
#include "nal_scanner.h"
#include "sps_parser.h"

namespace h264test {

// 把 RBSP 包成带 NAL 头的 NalUnit
inline h264::NalUnit wrapNal(uint8_t header,
                             const std::vector<uint8_t> &rbsp) {
  h264::NalUnit nal;
  nal.ebsp.push_back(header);
  auto body = h264::BitWriter::addEmulationBytes(rbsp);
  nal.ebsp.insert(nal.ebsp.end(), body.begin(), body.end());
  nal.nal_unit_type = header & 0x1Fu;
  nal.nal_ref_idc = (header >> 5) & 3u;
  return nal;
}

// 生成 High profile、1920x1080（含 1088->1080 裁剪）、30fps 的 SPS NAL。
// interlaced=true 时 frame_mbs_only=0 且打开 mbaff。
inline h264::NalUnit makeSps1080p(bool interlaced) {
  h264::BitWriter w;
  w.writeBits(100, 8);
  w.writeBits(0, 8);
  w.writeBits(40, 8);
  w.writeUE(0); // sps id
  w.writeUE(1); // chroma 4:2:0
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0); // 无 scaling list
  w.writeUE(0); // log2_max_frame_num_minus4 -> 16
  w.writeUE(0); // poc type 0
  w.writeUE(4); // log2_max_poc_lsb
  w.writeUE(2); // max ref frames
  w.writeBit(false);
  w.writeUE(119); // 1920
  w.writeUE(67);  // 1088
  w.writeBit(!interlaced);
  if (interlaced)
    w.writeBit(true); // mbaff
  w.writeBit(true); // direct 8x8
  w.writeBit(true); // cropping
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(4); // bottom crop -> 1080
  w.writeBit(true);  // vui present
  w.writeBit(false);
  w.writeBit(false);
  w.writeBit(false);
  w.writeBit(false);
  w.writeBit(true); // timing
  w.writeBits(1001, 32);
  w.writeBits(60060, 32);
  w.writeBit(true);
  w.writeRbspTrailingBits();
  return wrapNal(0x67, w.bytes());
}

inline h264::NalUnit makePps() {
  h264::BitWriter w;
  w.writeUE(0); // pps id
  w.writeUE(0); // sps id
  w.writeBit(false); // CAVLC
  w.writeBit(false);
  w.writeUE(0); // slice groups
  w.writeUE(0);
  w.writeUE(0);
  w.writeBit(false);
  w.writeBits(0, 2);
  w.writeSE(-3); // pic_init_qp_minus26
  w.writeSE(0);
  w.writeSE(0);
  w.writeBit(true);
  w.writeBit(false); // 无 redundant_pic_cnt
  w.writeRbspTrailingBits();
  return wrapNal(0x68, w.bytes());
}

} // namespace h264test
