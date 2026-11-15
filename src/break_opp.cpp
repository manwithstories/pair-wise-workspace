#include "break_opp.hpp"

#include <algorithm>
#include <cstdint>
#include <map>

namespace smx {
namespace {
bool is_space(std::uint32_t cp) { return cp == 0x20 || cp == 0x09; }
bool is_break_hyphen(std::uint32_t cp) { return cp == 0x2d || cp == 0x2010; }
bool is_zwsp(std::uint32_t cp) { return cp == 0x200b; }
bool is_hard(std::uint32_t cp) { return cp == '\n' || cp == '\r'; }

int priority(BreakReason r) {
  switch (r) {
    case BreakReason::Hard: return 0;
    case BreakReason::Space: return 1;
    case BreakReason::Hyphen: return 2;
    case BreakReason::Zwsp: return 3;
    case BreakReason::Cjk: return 4;
    default: return 5;
  }
}

void add(std::map<std::size_t, BreakReason>& out, std::size_t pos, BreakReason reason) {
  auto it = out.find(pos);
  if (it == out.end() || priority(reason) < priority(it->second))
    out[pos] = reason;
}
}  // namespace

bool is_line_start_forbidden(std::uint32_t cp) {
  switch (cp) {
    case 0x3001:
    case 0x3002:
    case 0xff0c:
    case 0x300d:
    case 0x300f:
    case 0xff1f:
    case 0xff01:
    case 0xff09:
    case 0x3011:
    case 0xff1a:
    case 0xff1b:
      return true;
    default:
      return false;
  }
}

bool is_line_end_forbidden(std::uint32_t cp) {
  switch (cp) {
    case 0xff08:
    case 0x300c:
    case 0x300e:
    case 0x3010:
      return true;
    default:
      return false;
  }
}

bool is_ideograph(std::uint32_t cp) {
  return (cp >= 0x3400 && cp <= 0x4dbf) || (cp >= 0x4e00 && cp <= 0x9fff) ||
         (cp >= 0xf900 && cp <= 0xfaff) || (cp >= 0x20000 && cp <= 0x2fa1f);
}

const char* break_reason_name(BreakReason reason) {
  switch (reason) {
    case BreakReason::End: return "end";
    case BreakReason::Space: return "space";
    case BreakReason::Hyphen: return "hyphen";
    case BreakReason::Zwsp: return "zwsp";
    case BreakReason::Cjk: return "cjk";
    case BreakReason::Hard: return "hard";
    case BreakReason::Forced: return "forced";
    case BreakReason::Ellipsis: return "ellipsis";
  }
  return "end";
}

BreakResult analyze_breaks(const std::vector<Cluster>& clusters) {
  BreakResult result;
  result.hard_after.assign(clusters.size(), false);
  std::map<std::size_t, BreakReason> candidates;

  for (std::size_t i = 0; i < clusters.size(); ++i) {
    const std::uint32_t cp = clusters[i].first_cp;
    if (is_hard(cp)) {
      result.hard_after[i] = true;
      add(candidates, i, BreakReason::Hard);
      continue;
    }
    std::size_t end = i + 1;
    std::uint32_t last = cp;
    while (end < clusters.size()) {
      const std::uint32_t next = clusters[end].first_cp;
      if (is_space(next) && is_space(last)) {
        last = next;
        ++end;
      } else {
        break;
      }
    }
    if (is_space(last)) add(candidates, end, BreakReason::Space);
    if (is_break_hyphen(cp)) add(candidates, i + 1, BreakReason::Hyphen);
    if (is_zwsp(cp)) add(candidates, i + 1, BreakReason::Zwsp);
    if (is_ideograph(cp)) {
      add(candidates, i + 1, BreakReason::Cjk);
      if (i > 0) add(candidates, i, BreakReason::Cjk);
    }
  }

  for (auto it = candidates.begin(); it != candidates.end();) {
    const std::size_t pos = it->first;
    if (pos == 0 || pos >= clusters.size()) {
      it = candidates.erase(it);
      continue;
    }
    const bool hard = it->second == BreakReason::Hard;
    const std::uint32_t before = clusters[pos - 1].first_cp;
    const std::uint32_t after = clusters[pos].first_cp;
    if (!hard && (is_line_start_forbidden(after) || is_line_end_forbidden(before) ||
                  is_hard(after))) {
      it = candidates.erase(it);
    } else {
      ++it;
    }
  }
  result.breaks.reserve(candidates.size());
  for (const auto& e : candidates) result.breaks.push_back({e.first, e.second});
  if (result.breaks.empty() && !clusters.empty())
    result.breaks.push_back({clusters.size(), BreakReason::End});
  return result;
}
}  // namespace smx
