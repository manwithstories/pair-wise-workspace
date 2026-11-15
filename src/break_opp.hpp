#pragma once
#include <cstddef>
#include <string>
#include <vector>
#include "utf8.hpp"

namespace smx {
enum class BreakReason {
  End,
  Space,
  Hyphen,
  Zwsp,
  Cjk,
  Hard,
  Forced,
  Ellipsis,
};

struct BreakPoint {
  std::size_t pos = 0;
  BreakReason reason = BreakReason::End;
};

struct BreakResult {
  std::vector<BreakPoint> breaks;
  std::vector<bool> hard_after;
};

BreakResult analyze_breaks(const std::vector<Cluster>& clusters);
const char* break_reason_name(BreakReason reason);
bool is_line_start_forbidden(std::uint32_t cp);
bool is_line_end_forbidden(std::uint32_t cp);
bool is_ideograph(std::uint32_t cp);
}  // namespace smx
