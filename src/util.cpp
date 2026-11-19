// 通用工具实现：SHA-256、JSON 写出、路径约束、目录与文件辅助。
#include "util.hpp"

#include <array>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <system_error>

namespace mb {

// ---------------- SHA-256（FIPS 180-4，自实现，纯整数运算） ----------------
namespace {
constexpr std::array<uint32_t, 64> kK = {
    0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu,
    0x59f111f1u, 0x923f82a4u, 0xab1c5ed5u, 0xd807aa98u, 0x12835b01u,
    0x243185beu, 0x550c7dc3u, 0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u,
    0xc19bf174u, 0xe49b69c1u, 0xefbe4786u, 0x0fc19dc6u, 0x240ca1ccu,
    0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau, 0x983e5152u,
    0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u,
    0x06ca6351u, 0x14292967u, 0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu,
    0x53380d13u, 0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u,
    0xa2bfe8a1u, 0xa81a664bu, 0xc24b8b70u, 0xc76c51a3u, 0xd192e819u,
    0xd6990624u, 0xf40e3585u, 0x106aa070u, 0x19a4c116u, 0x1e376c08u,
    0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au, 0x5b9cca4fu,
    0x682e6ff3u, 0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u,
    0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u};

inline uint32_t rotr(uint32_t x, uint32_t n) {
  return (x >> n) | (x << (32 - n));
}

struct Sha256Ctx {
  std::array<uint32_t, 8> h;
  uint64_t bitlen = 0;
  unsigned char buf[64];
  size_t buflen = 0;
  Sha256Ctx()
      : h{0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au,
         0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u} {}
  void block(const unsigned char* p) {
    uint32_t w[64];
    for (int i = 0; i < 16; ++i)
      w[i] = (uint32_t(p[i * 4]) << 24) | (uint32_t(p[i * 4 + 1]) << 16) |
             (uint32_t(p[i * 4 + 2]) << 8) | uint32_t(p[i * 4 + 3]);
    for (int i = 16; i < 64; ++i) {
      uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
      uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
      w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    uint32_t a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5],
             g = h[6], hh = h[7];
    for (int i = 0; i < 64; ++i) {
      uint32_t S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
      uint32_t ch = (e & f) ^ (~e & g);
      uint32_t t1 = hh + S1 + ch + kK[i] + w[i];
      uint32_t S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
      uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
      uint32_t t2 = S0 + maj;
      hh = g; g = f; f = e; e = d + t1; d = c; c = b; b = a; a = t1 + t2;
    }
    h[0] += a; h[1] += b; h[2] += c; h[3] += d; h[4] += e;
    h[5] += f; h[6] += g; h[7] += hh;
  }
  void update(const unsigned char* data, size_t len) {
    bitlen += uint64_t(len) * 8;
    while (len > 0) {
      size_t take = std::min(len, size_t(64) - buflen);
      std::memcpy(buf + buflen, data, take);
      buflen += take; data += take; len -= take;
      if (buflen == 64) { block(buf); buflen = 0; }
    }
  }
  std::string final_hex() {
    buf[buflen++] = 0x80;
    if (buflen > 56) {
      while (buflen < 64) buf[buflen++] = 0;
      block(buf); buflen = 0;
    }
    while (buflen < 56) buf[buflen++] = 0;
    for (int i = 7; i >= 0; --i) buf[buflen++] = (bitlen >> (i * 8)) & 0xff;
    block(buf);
    std::ostringstream os;
    for (uint32_t v : h) os << std::hex << std::setw(8) << std::setfill('0')
                            << static_cast<uint64_t>(v);
    return os.str();
  }
};
}  // namespace

std::string sha256_hex(const void* data, size_t len) {
  Sha256Ctx ctx;
  ctx.update(static_cast<const unsigned char*>(data), len);
  return ctx.final_hex();
}

std::string sha256_file(const std::filesystem::path& p, std::string* err) {
  std::ifstream f(p, std::ios::binary);
  if (!f) { if (err) *err = "cannot open file: " + p.string(); return {}; }
  Sha256Ctx ctx;
  std::array<char, 8192> chunk;
  while (f) {
    f.read(chunk.data(), chunk.size());
    std::streamsize n = f.gcount();
    if (n > 0) ctx.update(reinterpret_cast<unsigned char*>(chunk.data()),
                          static_cast<size_t>(n));
  }
  if (f.bad()) { if (err) *err = "read error: " + p.string(); return {}; }
  return ctx.final_hex();
}

// ---------------- 极简 JSON 写出器 ----------------
void JsonWriter::sep() {
  if (after_key_.back()) { after_key_.back() = false; return; }
  s_ << (top_.back() ? ',' : ' ');
  top_.back() = true;
}
void JsonWriter::key(const std::string& k) {
  sep();
  s_ << std::quoted(k) << ':';
  after_key_.back() = true;
}
void JsonWriter::val(const std::string& v) {
  sep(); s_ << std::quoted(v);
}
void JsonWriter::val(int64_t v) { sep(); s_ << v; }
void JsonWriter::val(double v) {
  sep();
  if (!std::isfinite(v)) v = 0.0;  // JSON 不允许 NaN/Inf
  s_ << std::fixed << std::setprecision(3) << v;
}
void JsonWriter::val(bool v) { sep(); s_ << (v ? "true" : "false"); }
void JsonWriter::begin_obj(const std::string& k) { key(k); begin_obj(); }
void JsonWriter::begin_obj() {
  sep(); s_ << '{';
  top_.push_back(false); open_.push_back('{'); after_key_.push_back(false);
}
void JsonWriter::begin_arr(const std::string& k) { key(k); begin_arr(); }
void JsonWriter::begin_arr() {
  sep(); s_ << '[';
  top_.push_back(false); open_.push_back('['); after_key_.push_back(false);
}
void JsonWriter::end() {
  char close = open_.back() == '{' ? '}' : ']';
  s_ << close;
  top_.pop_back();
  open_.pop_back();
  after_key_.pop_back();
}
std::string JsonWriter::str() const {
  std::string base = s_.str();
  // 补齐所有仍打开的层（含未 end 的根对象），逆序无关，括号类型按层取。
  for (size_t i = 0; i < open_.size(); ++i)
    base += (open_[i] == '{' ? '}' : ']');
  return base;
}

// ---------------- 路径与目录 ----------------
bool path_within(const std::filesystem::path& base,
                 const std::filesystem::path& p) {
  std::error_code ec;
  auto b = std::filesystem::weakly_canonical(base, ec);
  auto q = std::filesystem::weakly_canonical(p, ec);
  if (ec) return false;
  auto bs = b.string();
  auto qs = q.string();
  if (qs.size() < bs.size()) return false;
  if (qs.compare(0, bs.size(), bs) != 0) return false;
  return qs.size() == bs.size() || qs[bs.size()] == '/';
}

bool ensure_dir(const std::filesystem::path& p, std::string* err) {
  std::error_code ec;
  std::filesystem::create_directories(p, ec);
  if (ec) { if (err) *err = "mkdir failed: " + ec.message(); return false; }
  return true;
}

uint64_t file_size(const std::filesystem::path& p) {
  std::error_code ec;
  uint64_t n = std::filesystem::file_size(p, ec);
  return ec ? 0 : n;
}

}  // namespace mb
