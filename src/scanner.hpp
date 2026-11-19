// scanner.hpp -- libavformat front end with independent structural checks.
//
// The scanner is the only place that touches the input with FFmpeg.  It
// records per-track stream parameters and the container time base, streams
// packets in demux order with their absolute byte offsets, and cross-checks
// them against the ISO-BMFF tables parsed by mp4_structure.  Packets that the
// container promises but the file cannot hold are reported with the first
// byte that is missing and never handed to the writer.
#pragma once

#include <cstdint>
#include <functional>
#include <string>
#include <vector>

#include "config.hpp"
#include "media_clock.hpp"
#include "media_error.hpp"
#include "mp4_structure.hpp"

struct AVFormatContext;
struct AVPacket;

namespace mediaqc {

struct TrackSpec {
  int stream_index = -1;
  uint32_t track_id = 0;
  int media_type = 0;  // AVMediaType
  std::string codec_name;
  Rational time_base;
  Rational frame_rate;
  int width = 0;
  int height = 0;
  int sample_rate = 0;
  int channels = 0;
  int64_t container_samples = 0;   // from stsz when available
  int64_t container_duration = 0;  // stream duration in stream ticks
  bool index_entries_present = false;
  bool index_incomplete = false;
  int64_t first_bad_sample = -1;
  int64_t first_bad_offset = -1;
  int64_t samples_out_of_range = 0;
  bool decodable = false;
};

enum class InputKind {
  Ok,
  Empty,
  Unparsable,
  NoMoov,
  TruncatedMoov,
  TooLarge,
  TooManyTracks,
  Unsupported,
};

const char* input_kind_name(InputKind k);

struct ScanOutcome {
  InputKind kind = InputKind::Ok;
  bool fatal = false;             // no usable output can be produced
  Error fatal_error;              // valid when fatal
  std::vector<Error> errors;      // recovered warnings, in discovery order
  FileStructure structure;
  std::vector<TrackSpec> tracks;
  int64_t file_size = 0;
  std::string input_sha256;
  int64_t packets = 0;
  int64_t bytes_covered = 0;
  int64_t first_damaged_offset = -1;  // smallest error offset seen
  int64_t offset_uncertainty = 0;     // width of that error's uncertainty
  bool index_needs_rebuild = false;
  bool index_incomplete = false;      // stco/stsz cannot address all samples
  bool truncated_tail = false;
  bool fragmented = false;
  int64_t dropped_packets = 0;
};

// Called for every complete packet.  Returning false aborts streaming (the
// sink records its own reason in `why`).
using PacketCallback =
    std::function<bool(int stream_index, const AVPacket& packet, std::string& why)>;

class Scanner {
 public:
  Scanner() = default;
  ~Scanner();
  Scanner(const Scanner&) = delete;
  Scanner& operator=(const Scanner&) = delete;

  // Hashes the file, parses its structure and opens it with libavformat.
  // Returns false when no packet can be produced; `out.fatal_error` then
  // carries the localised reason.  Structural damage that leaves the media
  // readable is reported in `out` and returns true.
  bool open(const std::string& path, const Limits& limits, ScanOutcome& out);

  // Streams all packets through `cb`.  Updates `out` counters.  Returns false
  // when the callback aborted.
  bool stream(const PacketCallback& cb, ScanOutcome& out);

  AVFormatContext* context() const { return ctx_; }
  const std::string& path() const { return path_; }

 private:
  void collect_tracks(ScanOutcome& out);
  void validate_structure(ScanOutcome& out);
  void record(ScanOutcome& out, const Error& e);

  AVFormatContext* ctx_ = nullptr;
  std::string path_;
  std::vector<TrackSpec> tracks_;
};

}  // namespace mediaqc
