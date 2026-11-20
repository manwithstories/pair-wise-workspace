#include "relay/types.hpp"

#include <algorithm>
#include <cmath>
#include <sstream>

namespace relay {

std::string RelayError::to_string() const {
  std::ostringstream out;
  out << (severity == Severity::kError ? "error" : "warning");
  if (!ids.stage.empty()) {
    out << " stage=" << ids.stage;
  }
  if (!ids.track.empty()) {
    out << " track=" << ids.track;
  }
  if (ids.frame != 0) {
    out << " frame=" << ids.frame;
  }
  out << ": " << reason;
  if (!dependency_chain.empty()) {
    out << " [depends: " << dependency_chain << ']';
  }
  return out.str();
}

void ErrorSink::add(RelayError error) {
  events_.push_back(std::move(error));
}

void ErrorSink::warn(std::string stage, std::string track,
                     std::uint64_t frame, std::string reason,
                     std::string dependency_chain) {
  add(RelayError{Severity::kWarning, ExitCode::kOk,
                 Ids{std::move(stage), std::move(track), frame},
                 std::move(reason), std::move(dependency_chain)});
}

void ErrorSink::fail(std::string stage, std::string track,
                     std::uint64_t frame, std::string reason,
                     std::string dependency_chain) {
  add(RelayError{Severity::kError, ExitCode::kCorrupt,
                 Ids{std::move(stage), std::move(track), frame},
                 std::move(reason), std::move(dependency_chain)});
}

bool ErrorSink::has_errors() const {
  return std::any_of(events_.begin(), events_.end(),
                     [](const RelayError& e) {
                       return e.severity == Severity::kError;
                     });
}

bool ErrorSink::has_warnings() const {
  return std::any_of(events_.begin(), events_.end(),
                     [](const RelayError& e) {
                       return e.severity == Severity::kWarning;
                     });
}

int ErrorSink::exit_code() const {
  return has_errors() ? static_cast<int>(ExitCode::kCorrupt) : 0;
}

std::size_t ErrorSink::count(Severity severity) const {
  return static_cast<std::size_t>(
      std::count_if(events_.begin(), events_.end(),
                    [severity](const RelayError& e) {
                      return e.severity == severity;
                    }));
}

Micros Timebase::frames_to_us(std::int64_t frames) const {
  const std::int64_t numerator = frames * 1'000'000 * fps_den;
  const std::int64_t denominator = fps_num;
  // Round to nearest microsecond, symmetric around zero.
  const std::int64_t half = denominator / 2;
  return numerator >= 0 ? (numerator + half) / denominator
                        : -((-numerator + half) / denominator);
}

std::int64_t Timebase::us_to_frames(Micros us) const {
  const double frames = static_cast<double>(us) * fps_num /
                        (1'000'000.0 * fps_den);
  return static_cast<std::int64_t>(std::llround(frames));
}

}  // namespace relay
