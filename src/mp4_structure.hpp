// mp4_structure.hpp -- a minimal, independent ISO-BMFF box walker.
//
// FFmpeg is tolerant: it happily hands back a short final packet for a
// truncated mdat.  To report the first damaged byte and the last offset we
// could confirm, mediaqc walks the top-level boxes itself and checks each
// sample table against the real file size.  This module never writes to the
// input and never loads the whole file: it seeks.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media_error.hpp"

namespace mediaqc {

struct Box {
  int64_t offset = 0;  // absolute file offset of the box header
  int64_t size = 0;    // total size including header; <0 when declared size is 0
  bool size_64 = false;
  bool truncated = false;  // declared size runs past EOF
  char type[5] = {0, 0, 0, 0, 0};
  int depth = 0;
  std::string type_str() const { return type; }
};

struct ChunkRun {
  int64_t first_chunk = 1;  // 1-based, as stored by stsc
  int32_t samples_per_chunk = 0;
  int64_t first_sample = 0;  // 0-based index into stsz
};

struct SampleRef {
  int64_t offset = 0;
  int64_t size = 0;
};

struct TrackStructure {
  uint32_t track_id = 0;
  int stream_index = -1;
  char handler[5] = {0, 0, 0, 0, 0};
  bool is_video() const { return handler[0] == 'v'; }
  bool is_audio() const { return handler[0] == 's'; }
  int64_t moov_offset = -1;
  int64_t stbl_offset = -1;
  bool has_stbl = false;
  bool has_stco = false;  // stco or co64 present
  int64_t stco_offset = -1;
  int64_t stsz_offset = -1;
  int64_t nb_chunks = 0;
  int64_t nb_samples = 0;
  uint32_t timescale = 0;
  std::vector<int64_t> chunk_offsets;
  std::vector<ChunkRun> runs;
  std::vector<int32_t> sample_sizes;
  std::vector<SampleRef> samples;  // (offset, size) per sample, 0-based
  std::vector<int64_t> sample_dts; // from stts, 0-based
  int64_t index_entries_out_of_range = 0;
  int64_t first_bad_sample = -1;
  int64_t first_bad_offset = -1;
  bool truncated() const { return first_bad_sample >= 0; }
  bool usable_index() const { return has_stco && !samples.empty() && !truncated(); }
};

struct MdatBox {
  int64_t offset = 0;   // header offset
  int64_t header_size = 8;
  int64_t data_offset = 0;
  int64_t data_size = 0;  // declared, clamped to file size when truncated
  int64_t declared_size = 0;
  bool truncated = false;
  bool extends_past_eof() const { return truncated; }
};

struct FileStructure {
  int64_t file_size = 0;
  bool parsed_ftyp = false;
  int64_t moov_offset = -1;
  int64_t moov_size = 0;
  int64_t moof_offset = -1;
  int64_t mfra_offset = -1;
  bool has_moov = false;
  bool truncated = false;         // some declared box size ran past EOF
  bool moov_truncated = false;    // the moov box itself is cut short
  bool moov_after_mdat = false;   // moov is not in front of the media data
  int64_t last_confirmed_offset = 0;  // last offset whose bytes were validated
  int64_t first_truncated_offset = -1;
  std::string truncated_box;
  std::vector<Box> boxes;           // top level boxes, in file order
  std::vector<MdatBox> mdats;
  std::vector<TrackStructure> tracks;
  bool fragmented = false;
  bool parse_error = false;
  std::string parse_detail;
};

// Reads the file structure.  Returns false only on an unrecoverable I/O
// error, in which case `err` explains what went wrong; structurally damaged
// files still return true with the damage recorded in the structure.
bool read_mp4_structure(const std::string& path, FileStructure& out, Error& err);

// Walks the boxes of a byte range; exposed for tests.  `base` is the file
// offset of `data[0]`.  Populates `out` boxes at the given depth.
void walk_boxes(const uint8_t* data, int64_t len, int64_t base, int depth,
                std::vector<Box>& out);

// Expands (stco/co64, stsc, stsz) into per-sample (offset, size) pairs and
// flags the first sample that runs past the end of the file.
void build_sample_offsets(TrackStructure& track, int64_t file_size);

// True when the top-level layout has moov after any mdat.
bool moov_follows_media(const FileStructure& s);

}  // namespace mediaqc
