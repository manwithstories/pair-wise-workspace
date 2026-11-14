#include "annexb.h"

#include <cstring>
#include <vector>

namespace h264 {

namespace {

constexpr size_t kChunkSize = 1u << 20;

class Scanner {
public:
  Scanner(const NalCallback& cb, ScanStats& stats, size_t prefix_cap)
      : cb_(cb), stats_(stats), prefix_cap_(prefix_cap) {
        prefix_buf_ = new uint8_t[prefix_cap];
      }
  ~Scanner() { delete[] prefix_buf_; }

  void feed(const uint8_t* data, size_t n) {
    size_t i = 0;
    while (i < n) {
      const uint8_t* zero = static_cast<const uint8_t*>(memchr(data + i, 0x00, n - i));
      size_t run_end = zero ? static_cast<size_t>(zero - data) : n;
      append(data + i, run_end - i);
      pos_ += run_end - i;
      i = run_end;
      if (i >= n) {
        break;
      }
      size_t zeros = 0;
      while (i < n && data[i] == 0x00) {
        ++zeros;
        ++i;
      }
      if (i >= n) {
        pending_zeros_ += zeros;
        pos_ += zeros;
        break;
      }
      uint64_t zeros_total = pending_zeros_ + zeros;
      uint8_t b = data[i];
      if (zeros_total >= 2 && b == 0x01) {
        close_nal();
        stats_.start_codes++;
        in_nal_ = true;
        nal_offset_ = pos_ + zeros + 1;
        nal_len_ = 0;
        prefix_len_ = 0;
      } else {
        append_zeros(zeros_total);
        append(&b, 1);
      }
      pending_zeros_ = 0;
      pos_ += zeros + 1;
      ++i;
    }
  }

  void finish() {
    append_zeros(pending_zeros_);
    pending_zeros_ = 0;
    close_nal();
  }

private:
  void append(const uint8_t* p, size_t n) {
    if (!in_nal_ || n == 0) {
      return;
    }
    if (prefix_len_ < prefix_cap_) {
      size_t room = prefix_cap_ - prefix_len_;
      size_t take = n < room ? n : room;
      memcpy(prefix_buf_ + prefix_len_, p, take);
      prefix_len_ += take;
    }
    nal_len_ += n;
  }

  void append_zeros(uint64_t n) {
    static const uint8_t kZeros[16] = {0};
    while (n > 0) {
      size_t take = n > 16 ? 16 : static_cast<size_t>(n);
      append(kZeros, take);
      n -= take;
    }
  }

  void close_nal() {
    if (!in_nal_) {
      return;
    }
    NalUnit nal;
    nal.offset = nal_offset_;
    nal.length = nal_len_;
    if (nal_len_ == 0) {
      stats_.zero_length_nals++;
      nal.nal_type = 0xFF;
    } else {
      nal.header_byte = prefix_buf_[0];
      nal.nal_type = prefix_buf_[0] & 0x1F;
      nal.nal_ref_idc = (prefix_buf_[0] >> 5) & 0x03;
      nal.prefix = prefix_buf_ + 1;
      nal.prefix_len = prefix_len_ > 0 ? prefix_len_ - 1 : 0;
    }
    cb_(nal);
    in_nal_ = false;
  }

  const NalCallback& cb_;
  ScanStats& stats_;
  size_t prefix_cap_;
  uint8_t* prefix_buf_;
  size_t prefix_len_ = 0;
  uint64_t pos_ = 0;
  uint64_t pending_zeros_ = 0;
  uint64_t nal_offset_ = 0;
  uint64_t nal_len_ = 0;
  bool in_nal_ = false;
};

}  // namespace

void scan_annexb(FILE* fp, const NalCallback& cb, ScanStats& stats, size_t prefix_cap) {
  Scanner scanner(cb, stats, prefix_cap);
  std::vector<uint8_t> chunk(kChunkSize);
  size_t n;
  while ((n = fread(chunk.data(), 1, chunk.size(), fp)) > 0) {
    scanner.feed(chunk.data(), n);
    stats.bytes_scanned += n;
  }
  scanner.finish();
}

}  // namespace h264
