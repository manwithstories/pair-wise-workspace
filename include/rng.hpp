#pragma once

// xorshift64* PRNG. Self-contained so sample generation is reproducible on
// every platform and independent of <random> implementation details.

#include <cstdint>

namespace mfs {

class Rng {
 public:
  explicit Rng(uint64_t seed) : state_(seed == 0 ? 0x9E3779B97F4A7C15ull : seed) {}

  uint64_t next_u64() {
    uint64_t x = state_;
    x ^= x >> 12;
    x ^= x << 25;
    x ^= x >> 27;
    state_ = x;
    return x * 0x2545F4914F6CDD1Dull;
  }

  uint32_t next_u32() { return static_cast<uint32_t>(next_u64() >> 32); }

  // Uniform in [0, 1).
  double next_double() {
    return static_cast<double>(next_u64() >> 11) * (1.0 / 9007199254740992.0);
  }

  // Uniform in [0, bound).
  uint32_t below(uint32_t bound) {
    return bound == 0 ? 0 : static_cast<uint32_t>(next_u64() % bound);
  }

 private:
  uint64_t state_;
};

}  // namespace mfs
