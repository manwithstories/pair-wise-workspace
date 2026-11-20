#pragma once

#include <array>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "media/index.hpp"
#include "media/types.hpp"

namespace mediaqc {

// Writes the repaired delivery into one random-access MP4 container.  Packets
// are stream-copied (never re-encoded) so the output bytes for a given frame
// are exactly the bytes whose checksum the supplier signed; the encoder is
// only consulted for container metadata such as the time base and extradata.
//
// The writer also records, for every accepted frame, the byte offset and size
// of its payload inside the finished file, which becomes the random-access
// index.  Because MP4 writes the sample table after the media data, offsets
// are patched in once the muxer has flushed the trailer.
class IndexWriter {
 public:
  struct TrackSpec {
    TrackDesc desc;
    std::uint64_t frame_count = 0;
  };

  struct PacketInput {
    std::size_t track_index = 0;
    Ns time_ns = 0;          // repaired timestamp on the unified timeline
    std::uint64_t frame = 0;  // source sequence number
    bool keyframe = false;
    std::array<std::uint8_t, 32> sha256{};  // source frame checksum
    std::vector<std::uint8_t> payload;
  };

  IndexWriter();
  ~IndexWriter();
  IndexWriter(const IndexWriter&) = delete;
  IndexWriter& operator=(const IndexWriter&) = delete;

  // Creates the output container and streams.  `overwrite == false` refuses to
  // clobber an existing file.
  bool open(const std::string& output_path, const std::vector<TrackSpec>& tracks,
            bool overwrite, Error& error);

  // Appends one packet.  Packets may arrive in any track order; the muxer
  // interleaves them.  The returned entry is valid once close() succeeds.
  bool write(const PacketInput& packet, Error& error);

  // Finalizes the container, then fills in payload offsets and returns the
  // index.  On failure the partially written file is removed.
  bool close(MediaIndex& index, Error& error);

  // Abandons the output, removing whatever was written.
  void abort();

  [[nodiscard]] std::uint64_t packets_written() const { return packets_written_; }

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
  std::string output_path_;
  std::uint64_t packets_written_ = 0;
};

}  // namespace mediaqc
