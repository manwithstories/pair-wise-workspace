// verifier.hpp -- independent re-read of the written output.
//
// The verifier never trusts the writer's report: it reopens the output with
// libavformat, asks for a keyframe seek at several timeline points, decodes
// every packet, checks DTS monotonicity and compares the index entries it
// reads back with the journal that produced the file.  A decode failure is
// mapped back to the input byte range through the journal, and when the
// decoder cannot name a position inside a packet the whole packet's input
// range is reported (at most one packet wide).
#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "config.hpp"
#include "index_writer.hpp"
#include "media_error.hpp"

namespace mediaqc {

struct SeekProbe {
  int stream_index = -1;
  int64_t target_ticks = 0;   // canonical
  int64_t landed_dts = AV_NOPTS_VALUE;  // canonical
  int64_t landed_pts = AV_NOPTS_VALUE;
  int64_t landed_offset = -1;  // output byte offset
  bool keyframe = false;
  bool reachable = false;
  std::string note;
};

struct VerificationReport {
  bool ok = true;
  int64_t packets = 0;
  int64_t frames_decoded = 0;
  int64_t keyframes = 0;
  int64_t index_entries = 0;
  int64_t index_entries_checked = 0;
  int64_t index_errors = 0;
  int64_t decode_errors = 0;
  int64_t monotonic_errors = 0;
  int64_t seek_probes = 0;
  int64_t seek_failures = 0;
  std::vector<SeekProbe> probes;
  std::vector<UnindexableRange> unindexable;
  std::vector<Error> errors;
  int64_t first_error_offset = -1;
  int64_t offset_error_bytes = 0;  // uncertainty width of that error
  std::string output_sha256;
  int64_t output_bytes = 0;
  bool index_present = false;
};

// Verifies `output` against the journal that produced it.  `journal` may be
// empty to skip the input-offset mapping (the verifier still checks the
// output on its own).
bool verify_output(const std::filesystem::path& output,
                   const std::filesystem::path& journal,
                   const std::filesystem::path& input,
                   const Config& config, VerificationReport& report, Error& fatal);

}  // namespace mediaqc
