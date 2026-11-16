#include "nal_scanner.h"

#include <algorithm>
#include <istream>

namespace h264 {

void NalScanner::emitCurrent(std::vector<NalUnit> &out, bool truncated) {
  // 连续起始码会产生空负载：无 NAL 头字节，不是合法单元，直接丢弃。
  if (payload_.empty())
    return;
  NalUnit nal;
  nal.start_offset = cur_start_;
  nal.start_code_len = cur_sc_len_;
  nal.header_offset = cur_start_ + cur_sc_len_;
  const uint8_t header = payload_[0];
  nal.forbidden_zero_bit = (header >> 7) & 1u;
  nal.nal_ref_idc = (header >> 5) & 3u;
  nal.nal_unit_type = header & 0x1Fu;
  nal.ebsp = std::move(payload_);
  nal.truncated = truncated;
  payload_.clear();
  out.push_back(std::move(nal));
}

void NalScanner::feed(const uint8_t *data, size_t size,
                      std::vector<NalUnit> &out) {
  // 约定（与主流 Annex B 解析器一致）：
  //   零游程 + 01 构成起始码；zero_run==2 -> 3 字节码 00 00 01，
  //   zero_run>=3 -> 取末尾 3 个零构成 4 字节码 00 00 00 01，
  //   超出 3 的零视为上一 NAL 的尾零。暂存零在闭合前不进 payload_。
  for (size_t i = 0; i < size; ++i) {
    const uint8_t b = data[i];
    const uint64_t pos = total_pos_++;

    if (!in_nal_) {
      if (b == 0x00) {
        ++zero_run_;
        continue;
      }
      if (b == 0x01 && zero_run_ >= 2) {
        cur_sc_len_ = (zero_run_ >= 3 ? 4u : 3u);
        cur_start_ = pos - cur_sc_len_ + 1;
        in_nal_ = true;
        zero_run_ = 0;
        continue;
      }
      zero_run_ = 0; // 起始码前的杂散字节，忽略
      continue;
    }

    // NAL 负载状态
    if (b == 0x00) {
      ++zero_run_;
      continue; // 暂存连续零，等下一字节再判定
    }
    if (b == 0x01 && zero_run_ >= 2) {
      const unsigned leading = zero_run_;
      const uint32_t next_sc_len = (leading >= 3 ? 4u : 3u);
      // 超出起始码 3 个零的部分，归还为上一 NAL 的尾零
      const unsigned tail_zeros = leading >= 3 ? leading - 3 : 0;
      for (unsigned k = 0; k < tail_zeros; ++k)
        payload_.push_back(0x00);
      const uint64_t next_start = pos - next_sc_len + 1;
      emitCurrent(out, false);
      cur_start_ = next_start;
      cur_sc_len_ = next_sc_len;
      zero_run_ = 0;
      continue;
    }
    // 普通字节：冲刷暂存零（确属负载），再压当前字节
    while (zero_run_ > 0) {
      payload_.push_back(0x00);
      --zero_run_;
    }
    payload_.push_back(b);
  }
}

void NalScanner::finish(std::vector<NalUnit> &out) {
  if (!in_nal_)
    return;
  // EOF 前暂存的零属于最后一个 NAL 的负载
  while (zero_run_ > 0) {
    payload_.push_back(0x00);
    --zero_run_;
  }
  emitCurrent(out, true); // 未见下一起始码，标记截断
  in_nal_ = false;
}

std::vector<NalUnit> NalScanner::scanAll(const uint8_t *data, size_t size) {
  NalScanner s;
  std::vector<NalUnit> nals;
  s.feed(data, size, nals);
  s.finish(nals);
  return nals;
}

std::vector<NalUnit> NalScanner::scanStream(std::istream &in,
                                            size_t chunk_size) {
  std::vector<uint8_t> chunk(chunk_size);
  NalScanner s;
  std::vector<NalUnit> nals;
  while (in) {
    in.read(reinterpret_cast<char *>(chunk.data()),
            static_cast<std::streamsize>(chunk_size));
    const std::streamsize got = in.gcount();
    if (got > 0)
      s.feed(chunk.data(), static_cast<size_t>(got), nals);
    if (got < static_cast<std::streamsize>(chunk_size))
      break;
  }
  s.finish(nals);
  return nals;
}

void NalScanner::scanStreamCb(
    std::istream &in, const std::function<void(const NalUnit &)> &sink,
    size_t chunk_size) {
  std::vector<uint8_t> chunk(chunk_size);
  NalScanner s;
  std::vector<NalUnit> batch;
  while (in) {
    in.read(reinterpret_cast<char *>(chunk.data()),
            static_cast<std::streamsize>(chunk_size));
    const std::streamsize got = in.gcount();
    if (got > 0)
      s.feed(chunk.data(), static_cast<size_t>(got), batch);
    // 立即交付本批 NAL 并清空，驻留内存不随码流总长增长
    for (const NalUnit &n : batch)
      sink(n);
    batch.clear();
    if (got < static_cast<std::streamsize>(chunk_size))
      break;
  }
  s.finish(batch);
  for (const NalUnit &n : batch)
    sink(n);
}

} // namespace h264
