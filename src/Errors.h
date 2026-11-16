#pragma once
#include <stdexcept>

namespace lg {

// Exit codes: 0 ok, 2 args, 3 LUT parse, 4 I/O, 5 tolerance, 6 internal.
enum ExitCode {
  kExitOk = 0,
  kExitArgs = 2,
  kExitLutParse = 3,
  kExitIo = 4,
  kExitTolerance = 5,
  kExitInternal = 6,
};

struct ArgsError : std::runtime_error { using std::runtime_error::runtime_error; };
struct LutError : std::runtime_error { using std::runtime_error::runtime_error; };
struct IoError : std::runtime_error { using std::runtime_error::runtime_error; };
struct ToleranceError : std::runtime_error { using std::runtime_error::runtime_error; };
struct InternalError : std::runtime_error { using std::runtime_error::runtime_error; };

} // namespace lg
