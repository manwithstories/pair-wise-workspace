#pragma once

#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <functional>

namespace h264 {

// One NAL unit sliced out of an Annex B stream. `offset` is the absolute
// byte offset of the NAL header byte in the file; `length` counts header +
// payload, excluding the start code prefix. Only the first `prefix_len`
// payload bytes (<= prefix cap) are kept in memory.
struct NalUnit {
  uint8_t header_byte = 0;
  uint8_t nal_type = 0;
  uint8_t nal_ref_idc = 0;
  uint64_t offset = 0;
  uint64_t length = 0;
  const uint8_t* prefix = nullptr;
  size_t prefix_len = 0;
};

using NalCallback = std::function<void(const NalUnit&)>;

struct ScanStats {
  uint64_t start_codes = 0;
  uint64_t zero_length_nals = 0;
  uint64_t bytes_scanned = 0;
};

// Streams `fp` in fixed-size chunks, detects 3/4-byte start codes (with any
// number of leading zero bytes) and invokes `cb` once per NAL. Peak memory
// is bounded by the chunk size plus `prefix_cap`; the file is never fully
// loaded. Trailing bytes after the last start code are ignored.
void scan_annexb(FILE* fp, const NalCallback& cb, ScanStats& stats,
                 size_t prefix_cap = 256 * 1024);

}  // namespace h264
