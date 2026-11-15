#pragma once
#include <string>

namespace smx {
struct SelfTestResult {
  bool ok = false;
  std::string message;
};
SelfTestResult run_selftest();
}  // namespace smx
