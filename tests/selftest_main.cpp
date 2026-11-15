#include <iostream>
#include "selftest.hpp"
int main() {
  const auto result = smx::run_selftest();
  if (!result.ok) {
    std::cerr << "smx_selftest: FAIL: " << result.message << '\n';
    return 4;
  }
  std::cout << "smx_selftest: OK " << result.message << '\n';
  return 0;
}
