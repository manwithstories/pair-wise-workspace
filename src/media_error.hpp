// media_error.hpp -- the error model that threads through every stage.
//
// An Error always carries the input path, the container track id, the byte
// offset of the first affected byte, and (when the exact byte cannot be
// isolated) the exclusive end of the uncertainty span.  `action` records what
// the pipeline did about it; nothing is dropped silently.
#pragma once

#include <cstdint>
#include <string>

namespace mediaqc {

enum class Stage {
  Open,
  Structure,
  Scan,
  Timeline,
  Write,
  Index,
  Verify,
};

enum class Severity {
  Recovered,  // output was still produced and verified
  Fatal,      // this asset could not be repaired unambiguously
};

enum class Code {
  None,
  FileEmpty,
  HeaderUnparsable,
  NoMoov,
  MoovTruncated,
  BoxTruncated,
  MdatTruncated,
  PacketTruncated,
  PacketOutOfBounds,
  IndexEntryOutOfRange,
  IndexRebuiltFromSizes,
  IndexMissing,
  DecodeError,
  DecodeDependentDropped,
  TimestampMissing,
  TimestampBackwards,
  TimestampWrap,
  TimestampAmbiguous,
  TrackLimitExceeded,
  PacketTooLarge,
  FileTooLarge,
  PathEscape,
  UnsupportedCodec,
  OutputWriteFailed,
  OutputVerifyFailed,
  CheckpointMismatch,
  CheckpointCorrupt,
  EmptyOutput,
  UnindexableRange,
  OffsetUnknown,
};

const char* stage_name(Stage s);
const char* code_name(Code c);
const char* severity_name(Severity s);

struct Error {
  std::string file;           // input path as given on the command line
  int track = -1;             // container track id (tkhd track_ID), -1 unknown
  int stream_index = -1;      // demuxer stream index, -1 unknown
  Stage stage = Stage::Open;
  Severity severity = Severity::Fatal;
  Code code = Code::None;

  int64_t byte_offset = 0;      // first affected byte, absolute in the input
  int64_t byte_offset_end = 0;  // exclusive end of uncertainty span, 0 = exact
  int64_t last_confirmed = -1;  // last offset whose contents were validated
  bool offset_known = true;

  int64_t pts = -1;  // affected timestamp in stream ticks, -1 if unknown
  int64_t dts = -1;
  std::string time_base;  // "num/den" when pts/dts are set

  std::string detail;
  std::string action;  // recovery action taken or required

  // Localisation uncertainty in bytes: 0 when the offset is exact, otherwise
  // the width of the range.  A decode error mapped to a whole packet reports
  // that packet's size, which is <= 1 packet by construction.
  int64_t offset_uncertainty() const {
    if (!offset_known) return 0;
    return byte_offset_end > byte_offset ? byte_offset_end - byte_offset : 0;
  }

  void set_range(int64_t begin, int64_t end) {
    byte_offset = begin;
    byte_offset_end = end > begin ? end : 0;
  }
};

}  // namespace mediaqc
