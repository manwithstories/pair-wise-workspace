// index_writer.hpp -- incremental payload writer and index-rebuilding muxer.
//
// Streaming and muxing are deliberately split so that an interrupted batch
// can resume without redoing timestamp repair or copying payload again:
//
//   1. The streaming pass appends kept packet payloads to `payload` and one
//      fixed-size record per packet to `journal`.  Both are append-only files,
//      so the committed length is a valid checkpoint.
//   2. `mux_output` replays the journal through libavformat with the index in
//      front of the media (faststart), which reconstructs a seekable index
//      from the packet boundaries we recorded -- including for inputs whose
//      moov was at the end, missing, or contained out-of-range entries.
//
// The journal is also the mapping used by the verifier: journal record N of a
// stream is output packet N of that stream, so a decode error in the output
// can name the exact input byte range it came from.
#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "config.hpp"
#include "media_clock.hpp"
#include "media_error.hpp"

namespace mediaqc {

struct JournalEntry {
  uint32_t stream_index = 0;
  uint32_t flags = 0;      // AV_PKT_FLAG_KEY / CORRUPT / DISCARD
  uint32_t size = 0;
  uint32_t reserved = 0;
  int64_t dts = AV_NOPTS_VALUE;   // repaired, stream time base
  int64_t pts = AV_NOPTS_VALUE;
  int64_t duration = 0;
  int64_t src_offset = -1;        // input byte offset, -1 when unknown
  int64_t payload_offset = 0;     // offset of the payload in the payload file
};

inline constexpr int64_t kJournalRecordBytes = 48;

std::string serialize_journal_entry(const JournalEntry& e);
bool parse_journal_entry(const char* data, JournalEntry& e);

// One continuous span of the output timeline that cannot be reached by
// seeking: no keyframe at its start, or no samples at all.
struct UnindexableRange {
  int stream_index = -1;
  int64_t start_ticks = 0;  // canonical 1/90000
  int64_t end_ticks = 0;
  std::string reason;
};

struct MuxReport {
  int64_t packets_written = 0;
  int64_t skipped_packets = 0;  // rejected by the muxer (bad timestamp)
  int64_t index_entries = 0;
  int64_t keyframes = 0;
  int64_t bytes_written = 0;
  std::string output_sha256;
  std::vector<UnindexableRange> unindexable;
  std::vector<Error> errors;
  bool ok = true;
};

// Append-only payload+journal writer used by the streaming pass.
class IndexWriter {
 public:
  IndexWriter() = default;
  ~IndexWriter();

  // Opens (creating or appending) `<dir>/<stem>.payload` and `.journal`.
  // When resuming, existing file lengths must match the checkpoint; a
  // mismatch refuses the resume.
  // `reset` discards any previous partial work (fresh run); otherwise the
  // files are reopened at their committed length and checked against the
  // checkpoint's expectations.
  bool open(const std::filesystem::path& dir, const std::string& stem,
            int64_t expect_payload_bytes, int64_t expect_journal_bytes, bool reset,
            std::string& err);

  bool append(const JournalEntry& entry, const uint8_t* data, std::string& err);
  bool flush(std::string& err);  // fsyncs both files
  void close_files();

  const std::filesystem::path& payload_path() const { return payload_path_; }
  const std::filesystem::path& journal_path() const { return journal_path_; }
  int64_t payload_bytes() const { return payload_bytes_; }
  int64_t journal_bytes() const { return journal_bytes_; }
  int64_t packets() const { return packets_; }

 private:
  std::filesystem::path payload_path_;
  std::filesystem::path journal_path_;
  int payload_fd_ = -1;
  int journal_fd_ = -1;
  int64_t payload_bytes_ = 0;
  int64_t journal_bytes_ = 0;
  int64_t packets_ = 0;
};

// Replays payload+journal into a seekable MP4 with a leading moov.  `input` is
// opened only for codec parameters and stream order; no packet is read from
// it.  Writes to `<output>.partial` and returns its hash; the caller renames
// it into place after verification.
struct MuxOptions {
  Config config;
  // Time span (canonical ticks) after which a missing keyframe makes the
  // following span unindexable; 0 -> 2 s.
  int64_t keyframe_gap_ticks = 0;
};

bool mux_output(const std::filesystem::path& input,
                const std::filesystem::path& payload,
                const std::filesystem::path& journal,
                const std::filesystem::path& output_partial,
                const MuxOptions& options, MuxReport& report, Error& err);

// Reads all journal records (bounded by `max_records`).
bool read_journal(const std::filesystem::path& journal, int64_t max_records,
                  std::vector<JournalEntry>& out, std::string& err);

}  // namespace mediaqc
