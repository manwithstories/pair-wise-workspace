#pragma once

#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <string>
#include <vector>

namespace h264 {

enum class ErrCode { kParse, kUnsupported };

class Error : public std::runtime_error {
public:
  Error(ErrCode code, const std::string& msg) : std::runtime_error(msg), code_(code) {}
  ErrCode code() const { return code_; }

private:
  ErrCode code_;
};

[[noreturn]] inline void fail_parse(const std::string& msg) { throw Error(ErrCode::kParse, msg); }
[[noreturn]] inline void fail_unsupported(const std::string& msg) {
  throw Error(ErrCode::kUnsupported, msg);
}

// Bit-level reader over one NAL payload. The constructor strips emulation
// prevention bytes (00 00 03 -> 00 00) so all reads operate on RBSP.
class BitReader {
public:
  BitReader(const uint8_t* ebsp, size_t size);

  uint32_t read_bit();
  uint32_t read_bits(int n);          // n in [0, 32], MSB first
  uint32_t read_ue();                 // unsigned Exp-Golomb
  int32_t read_se();                  // signed Exp-Golomb
  uint32_t read_te(uint32_t range);   // truncated Exp-Golomb

  bool more_rbsp_data() const;
  size_t bit_position() const { return bit_pos_; }
  size_t byte_offset() const { return bit_pos_ / 8; }  // absolute, within RBSP
  int last_ue_bits() const { return last_ue_bits_; }   // code length of last ue/se

private:
  std::vector<uint8_t> rbsp_;
  size_t bit_pos_ = 0;
  int last_ue_bits_ = 0;
};

}  // namespace h264
