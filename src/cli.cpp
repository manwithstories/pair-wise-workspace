#include "relay/cli.hpp"

#include <charconv>
#include <cstdio>
#include <iostream>
#include <sstream>

#include "relay/checkpoint.hpp"

namespace relay::cli {
namespace {

bool parse_uint(const std::string& text, std::uint64_t& value) {
  std::size_t begin = 0;
  while (begin < text.size() && text[begin] == '+') {
    ++begin;
  }
  if (begin == text.size()) {
    return false;
  }
  const auto [ptr, ec] =
      std::from_chars(text.data() + begin, text.data() + text.size(), value);
  return ec == std::errc{} && ptr == text.data() + text.size();
}

std::vector<std::string> split_csv(const std::string& text) {
  std::vector<std::string> parts;
  std::string current;
  std::istringstream stream(text);
  while (std::getline(stream, current, ',')) {
    if (!current.empty()) {
      parts.push_back(current);
    }
  }
  return parts;
}

std::string format_double(double value, int precision) {
  char buffer[64];
  std::snprintf(buffer, sizeof(buffer), "%.*f", precision, value);
  return buffer;
}

int usage_error(const std::string& message) {
  std::cerr << "relay: " << message << "\n\n" << usage();
  return static_cast<int>(ExitCode::kUsage);
}

}  // namespace

std::string usage() {
  return R"(usage:
  relay demo   [--seed N] [--loss-burst N] [--bandwidth K,K,K]
               [--segment-ms N] [--ladder 360p,720p,1080p] [--verify]
               [--work DIR] [--frames N] [--gop N] [--cams a,b,c]
               [--corrupt] [--lose-keyframe] [--empty-track]
               [--interrupt-after-segment N]
  relay resume [--work DIR] [--verify]

Exit codes: 0 success, 2 usage error, 3 corrupt input or unrecoverable
dependency.  `demo` regenerates its own fixtures under --work (default
./relay-work); `resume` continues the last interrupted run.
)";
}

CommandLine parse(const std::vector<std::string>& arguments) {
  CommandLine result;
  result.config.work_dir = "relay-work";
  if (arguments.empty()) {
    result.error = "missing command";
    return result;
  }
  result.command = arguments[0];
  if (result.command == "--help" || result.command == "-h" ||
      result.command == "help") {
    result.command = "help";
    result.show_help = true;
    return result;
  }
  if (result.command == "resume") {
    result.config.resume = true;
  } else if (result.command != "demo") {
    result.error = "unknown command '" + result.command + "'";
    return result;
  }

  auto next = [&](std::size_t& i, const std::string& flag) -> std::string {
    if (i + 1 >= arguments.size()) {
      result.error = "missing value for " + flag;
      return {};
    }
    return arguments[++i];
  };

  for (std::size_t i = 1; i < arguments.size() && result.error.empty(); ++i) {
    const std::string& flag = arguments[i];
    std::uint64_t number = 0;
    if (flag == "--help" || flag == "-h") {
      result.show_help = true;
      return result;
    } else if (flag == "--verify") {
      result.config.verify = true;
    } else if (flag == "--corrupt") {
      result.config.corrupt = true;
    } else if (flag == "--lose-keyframe") {
      result.config.lose_keyframe = true;
    } else if (flag == "--empty-track") {
      result.config.empty_track = true;
    } else if (flag == "--seed") {
      const std::string value = next(i, flag);
      if (value.empty() || !parse_uint(value, number)) {
        result.error = "--seed requires a non-negative integer";
      } else {
        result.config.seed = static_cast<unsigned>(number);
      }
    } else if (flag == "--frames") {
      const std::string value = next(i, flag);
      if (value.empty() || !parse_uint(value, number) || number == 0 ||
          number > 100000) {
        result.error = "--frames requires an integer in 1..100000";
      } else {
        result.config.frames = static_cast<int>(number);
      }
    } else if (flag == "--gop") {
      const std::string value = next(i, flag);
      if (value.empty() || !parse_uint(value, number) || number == 0) {
        result.error = "--gop requires a positive integer";
      } else {
        result.config.gop = static_cast<int>(number);
      }
    } else if (flag == "--loss-burst") {
      const std::string value = next(i, flag);
      if (value.empty() || !parse_uint(value, number)) {
        result.error = "--loss-burst requires a non-negative integer";
      } else {
        result.config.loss_burst = static_cast<int>(number);
      }
    } else if (flag == "--segment-ms") {
      const std::string value = next(i, flag);
      if (value.empty() || !parse_uint(value, number) || number == 0) {
        result.error = "--segment-ms requires a positive integer";
      } else {
        result.config.segment_ms = static_cast<Micros>(number);
      }
    } else if (flag == "--interrupt-after-segment") {
      const std::string value = next(i, flag);
      if (value.empty() || !parse_uint(value, number)) {
        result.error =
            "--interrupt-after-segment requires a non-negative integer";
      } else {
        result.config.interrupt_after_segment = true;
        result.config.interrupt_segment = static_cast<std::size_t>(number);
      }
    } else if (flag == "--bandwidth") {
      const std::string value = next(i, flag);
      std::vector<std::uint64_t> bandwidth;
      for (const auto& part : split_csv(value)) {
        std::uint64_t kbps = 0;
        if (!parse_uint(part, kbps) || kbps == 0) {
          result.error = "--bandwidth expects comma-separated kbps values";
          break;
        }
        bandwidth.push_back(kbps);
      }
      if (bandwidth.empty() && result.error.empty()) {
        result.error = "--bandwidth requires at least one value";
      }
      result.config.bandwidth = bandwidth;
    } else if (flag == "--ladder") {
      const std::string value = next(i, flag);
      std::vector<std::string> names;
      for (const auto& part : split_csv(value)) {
        if (part != "360p" && part != "480p" && part != "720p" &&
            part != "1080p") {
          result.error = "unknown rendition '" + part + "' in --ladder";
          break;
        }
        names.push_back(part);
      }
      if (names.empty() && result.error.empty()) {
        result.error = "--ladder requires at least one rendition";
      }
      result.config.ladder = names;
    } else if (flag == "--cams") {
      const std::string value = next(i, flag);
      const auto names = split_csv(value);
      if (names.empty()) {
        result.error = "--cams requires at least one camera id";
      }
      result.config.cameras = names;
    } else if (flag == "--work") {
      const std::string value = next(i, flag);
      if (value.empty()) {
        result.error = "--work requires a directory";
      } else {
        result.config.work_dir = value;
      }
    } else {
      result.error = "unknown option '" + flag + "'";
    }
  }
  return result;
}

int execute(const CommandLine& command_line) {
  if (command_line.show_help) {
    std::cout << usage();
    return 0;
  }
  if (!command_line.error.empty()) {
    return usage_error(command_line.error);
  }
  PipelineResult result = command_line.command == "resume"
                              ? run_pipeline(command_line.config)
                              : run_demo(command_line.config);
  const PipelineReport& report = result.report;

  for (const auto& event : result.errors) {
    if (event.severity == Severity::kError) {
      std::cerr << event.to_string() << '\n';
    }
  }
  if (report.interrupted) {
    std::cout << "interrupted=true\n";
    std::cout << "resume_hint=relay resume --work " << command_line.config.work_dir
              << '\n';
    std::cout << "completed_stages=" << report.stage_order.size() << '\n';
    for (const auto& stage : report.stage_order) {
      std::cout << "stage=" << stage << " status=done\n";
    }
    std::cout << "exit_code=0\n";
    return 0;
  }
  std::cout << "input_digest=" << report.input_digest.substr(0, 16) << '\n';
  std::cout << "tracks=" << report.tracks << '\n';
  std::cout << "usable_tracks=" << report.usable_tracks << '\n';
  std::cout << "empty_tracks=" << report.empty_tracks.size() << '\n';
  std::cout << "blocked_tracks=" << report.blocked_tracks.size() << '\n';
  for (const auto& blocked : report.blocked_tracks) {
    std::cout << "blocked=" << blocked << '\n';
  }
  std::cout << "segments=" << report.segments << '\n';
  std::cout << "random_access_segments=" << report.random_access_segments << '\n';
  std::cout << "unseekable_segments=" << report.unseekable_segments << '\n';
  std::cout << "duplicates=" << report.duplicates << '\n';
  std::cout << "reordered=" << report.reordered << '\n';
  std::cout << "lost_burst=" << report.lost_burst << '\n';
  std::cout << "late_dropped=" << report.late_dropped << '\n';
  std::cout << "epochs=" << report.epochs << '\n';
  std::cout << "checksum_failures=" << report.checksum_failures << '\n';
  std::cout << "quarantined_frames=" << report.quarantined_frames << '\n';
  std::cout << "downgrades=" << report.downgrades << '\n';
  std::cout << "resumed_stages=" << report.resumed_stages << '\n';
  std::cout << "recomputed_stages=" << report.recomputed_stages << '\n';
  std::cout << "keyframe_hit_rate="
            << format_double(report.keyframe_hit_rate, 3) << '\n';
  std::cout << "aligned_cut_error_frames="
            << report.aligned_cut_error_frames << '\n';
  std::cout << "roundtrip_rate=" << format_double(report.roundtrip_rate, 3)
            << '\n';
  std::cout << "peak_buffer_bytes=" << report.peak_buffer_bytes << '\n';
  std::cout << "manifest_digest=" << report.manifest_digest.substr(0, 16)
            << '\n';
  std::cout << "manifest=" << result.manifest_path.string() << '\n';
  std::cout << "segment_hashes=";
  for (std::size_t i = 0; i < report.segment_hashes.size(); ++i) {
    if (i != 0) {
      std::cout << ',';
    }
    std::cout << report.segment_hashes[i];
  }
  std::cout << '\n';
  if (report.resumed) {
    std::cout << "resumed=true\n";
  }
  if (report.resumed_stages + report.recomputed_stages > 0) {
    for (const auto& stage : report.stage_order) {
      std::cout << "stage=" << stage << " status=done\n";
    }
  }
  std::cout << "exit_code=" << result.exit_code << '\n';
  // The exit code is the process contract; a zero round-trip or a failed
  // verification is a failure even if every other number looks healthy.
  if (result.exit_code == 0 &&
      (report.roundtrip_rate != 1.0 || report.segments == 0)) {
    if (command_line.config.verify) {
      std::cerr << "relay: verification failed (roundtrip_rate != 1.0 or no "
                   "segments produced)\n";
    }
    return static_cast<int>(ExitCode::kCorrupt);
  }
  return result.exit_code;
}

}  // namespace relay::cli
