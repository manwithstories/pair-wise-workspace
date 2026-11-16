#pragma once
#include <string>
#include <utility>

// Error taxonomy shared by every module.
//
// Exit codes are the contract from the spec:
//   0 success, 2 parameter/input-format error, 3 colour-space/matrix error,
//   4 render failure, 5 the two render paths disagree beyond the threshold.
namespace lutgrade {

enum class ExitCode : int {
  kOk = 0,
  kUsage = 2,
  kColor = 3,
  kRender = 4,
  kMismatch = 5,
};

// Thrown by parsing / validation / rendering code and translated into the
// matching exit code in main(). Carries a stable machine-ish `kind` so
// callers can distinguish e.g. "missing LUT_3D_SIZE" from "size not cubic".
class Error {
 public:
  Error(ExitCode code, std::string kind, std::string message)
      : code_(code), kind_(std::move(kind)), message_(std::move(message)) {}

  ExitCode code() const noexcept { return code_; }
  const std::string& kind() const noexcept { return kind_; }
  const std::string& message() const noexcept { return message_; }

 private:
  ExitCode code_;
  std::string kind_;
  std::string message_;
};

}  // namespace lutgrade
