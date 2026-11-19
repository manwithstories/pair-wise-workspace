#include "bitstream.hpp"

#include <algorithm>
#include <array>
#include <fstream>
#include <sstream>
#include <map>
#include <optional>
#include <set>

#include "json.hpp"

namespace media_restore {
namespace {

constexpr size_t kAnnexBChunk = 4 * 1024 * 1024;

Diagnostic make(ErrorCode code, std::string message, const Location& location) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.message = std::move(message);
  diagnostic.location = location;
  return diagnostic;
}

// H.264: NAL type 5 = IDR, 7 = SPS (a new SPS implies a new access unit but
// not necessarily a keyframe, so only IDR/type 5 counts as a keyframe).
bool h264_keyframe(int nal_type) { return nal_type == 5; }
// H.265: IRAP types 16..21 (BLA/IDR/CRA).
bool h265_keyframe(int nal_type) { return nal_type >= 16 && nal_type <= 21; }
// AV1: OBU_FRAME (6) whose frame header reports key_frame == 1.
bool av1_obu_is_frame(int obu_type) { return obu_type == 6; }

struct AnnexBState {
  Codec codec = Codec::h264;
  Limits limits;
  StreamScan* out = nullptr;
  std::vector<Diagnostic>* diagnostics = nullptr;

  int64_t last_start = -1;  // absolute offset of the last start code processed
  int64_t unit_start = -1;
  int64_t au_start = -1;         // start of the current access unit
  int64_t pending_au_start = -1; // first non-VCL unit since the last AU
  int unit_type = -1;
  bool failed = false;

  void emit(int64_t end_offset) {
    if (unit_start < 0) return;
    StreamUnit unit;
    unit.offset = unit_start;
    unit.au_offset = au_start;
    unit.starts_au = unit_start == au_start;
    unit.length = end_offset - unit_start;
    unit.nal_type = unit_type;
    unit.keyframe = codec == Codec::h264 ? h264_keyframe(unit_type)
                                         : h265_keyframe(unit_type);
    out->units.push_back(unit);
    if (unit.keyframe) {
      // For Annex B, an index records the start of the access unit (the
      // parameter sets preceding the IDR/IRAP slice), not the slice itself.
      StreamUnit keyframe = unit;
      keyframe.offset = unit.au_offset;
      out->keyframes.push_back(keyframe);
    }
    unit_start = -1;
  }

  int classify(unsigned char first) const {
    return codec == Codec::h264 ? (first & 0x1F) : ((first >> 1) & 0x3F);
  }

  // VCL slice types begin a new access unit when they are the first slice
  // of the picture.  Without parsing slice headers we use the encoder
  // convention (one slice per picture) and treat every VCL unit as an AU
  // start; non-VCL parameter sets are grouped into the AU they precede.
  bool starts_access_unit(int type) const {
    if (codec == Codec::h264) return type >= 1 && type <= 5;
    // HEVC VCL types 0..9 and IRAP types 16..21 both carry a picture.
    return type <= 9 || (type >= 16 && type <= 21);
  }

  // Processes one buffer that starts at absolute offset `buffer_abs`.
  // Start codes already handled (offset <= last_start) are skipped; this is
  // how the 3-byte carry across chunk boundaries stays idempotent.
  bool process(std::string_view buffer, int64_t buffer_abs) {
    size_t i = 0;
    while (true) {
      size_t found = std::string_view::npos;
      for (; i + 3 <= buffer.size(); ++i) {
        if (buffer[i] == 0 && buffer[i + 1] == 0 && buffer[i + 2] == 1) {
          found = i;
          break;
        }
      }
      if (found == std::string_view::npos) return true;
      size_t header = found + 3;
      if (header >= buffer.size()) return true;  // type byte in the next chunk
      const int64_t abs = buffer_abs + static_cast<int64_t>(found);
      if (abs <= last_start) {
        i = header;
        continue;
      }
      // A four-byte start code (00 00 00 01) starts one byte earlier; record
      // the leading zero so offsets match encoder-written indexes.
      int64_t start_abs = abs;
      if (found > 0 && buffer[found - 1] == 0 &&
          (found < 2 || buffer[found - 2] != 0)) {
        start_abs = abs - 1;
      }
      if (unit_start >= 0) emit(start_abs);
      unit_start = start_abs;
      unit_type = classify(static_cast<unsigned char>(buffer[header]));
      if (starts_access_unit(unit_type)) {
        // Parameter sets that precede a picture belong to that picture's
        // access unit, which is the offset an encoder writes to its index.
        au_start = pending_au_start >= 0 ? pending_au_start : start_abs;
        pending_au_start = -1;
      } else {
        // Leading non-VCL units before the first picture also belong to the
        // first access unit, so remember them but keep the current AU start.
        if (au_start < 0) au_start = start_abs;
        if (pending_au_start < 0) pending_au_start = start_abs;
      }
      last_start = abs;
      if (out->units.size() + 1 > limits.max_index_entries) {
        diagnostics->push_back(make(ErrorCode::index_too_large,
                                    "raw stream has more units than the limit of " +
                                        std::to_string(limits.max_index_entries),
                                    {.byte_offset = start_abs}));
        failed = true;
        return false;
      }
      i = header;
    }
  }
};

bool scan_annexb(std::istream& stream, int64_t size, Codec codec,
                 const Limits& limits, StreamScan* out,
                 std::vector<Diagnostic>* diagnostics) {
  if (size > limits.max_media_bytes) {
    diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                "raw stream exceeds the media size limit", {}));
    return false;
  }

  AnnexBState state;
  state.codec = codec;
  state.limits = limits;
  state.out = out;
  state.diagnostics = diagnostics;

  // Keep the trailing 4 bytes between chunks: a start code may straddle a
  // chunk boundary and its type byte may be the first byte of the next chunk.
  constexpr size_t kCarry = 4;
  std::string carry;
  int64_t consumed = 0;
  std::string chunk(kAnnexBChunk, '\0');
  while (true) {
    stream.read(chunk.data(), static_cast<std::streamsize>(chunk.size()));
    const auto read = static_cast<size_t>(stream.gcount());
    if (read == 0) break;
    std::string buffer = carry;
    buffer.append(chunk.data(), read);
    const int64_t buffer_abs = consumed - static_cast<int64_t>(carry.size());
    if (!state.process(buffer, buffer_abs)) return false;
    consumed += static_cast<int64_t>(read);
    if (consumed > limits.max_media_bytes) {
      diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                  "raw stream exceeds the media size limit", {}));
      return false;
    }
    const size_t keep = std::min(kCarry, buffer.size());
    carry.assign(buffer.end() - static_cast<long>(keep), buffer.end());
  }
  if (stream.bad()) {
    diagnostics->push_back(make(ErrorCode::io, "read failed while scanning", {}));
    return false;
  }
  state.emit(consumed);

  out->codec = codec;
  out->stream_size = consumed;
  out->unit_count = out->units.size();
  for (size_t i = 1; i < out->units.size(); ++i) {
    if (out->units[i].offset <= out->units[i - 1].offset) {
      diagnostics->push_back(make(ErrorCode::internal,
                                  "scan produced out-of-order units",
                                  {.byte_offset = out->units[i].offset}));
      return false;
    }
  }
  return true;
}

// OBU size fields are LEB128 encoded (AV1 spec 4.10.5).
int leb128(const std::string& buffer, size_t pos, size_t end, uint64_t* value) {
  uint64_t result = 0;
  int shift = 0;
  while (pos < end && shift <= 56) {
    const uint8_t byte = static_cast<uint8_t>(buffer[pos++]);
    result |= static_cast<uint64_t>(byte & 0x7F) << shift;
    if ((byte & 0x80) == 0) {
      *value = result;
      return static_cast<int>(pos);
    }
    shift += 7;
  }
  return -1;
}

bool scan_av1(const std::string& buffer, const Limits& limits, StreamScan* out,
              std::vector<Diagnostic>* diagnostics) {
  const auto size = static_cast<int64_t>(buffer.size());

  size_t pos = 0;
  bool fresh_temporal_unit = true;
  int64_t temporal_unit_start = 0;
  while (pos < buffer.size()) {
    const size_t header = pos;
    const uint8_t byte = static_cast<uint8_t>(buffer[pos]);
    if (byte & 0x80) {
      diagnostics->push_back(make(ErrorCode::internal,
                                  "AV1 forbidden header bit set",
                                  {.byte_offset = static_cast<int64_t>(pos)}));
      return false;
    }
    const int type = (byte >> 3) & 0x0F;
    const bool extension = (byte & 0x04) != 0;
    const bool has_size = (byte & 0x02) != 0;
    ++pos;
    if (extension) {
      if (pos + 1 > buffer.size()) break;
      pos += 1;
    }
    uint64_t payload = 0;
    if (has_size) {
      const int next = leb128(buffer, pos, buffer.size(), &payload);
      if (next < 0) {
        diagnostics->push_back(make(ErrorCode::internal,
                                    "AV1 OBU size field is malformed",
                                    {.byte_offset = static_cast<int64_t>(pos)}));
        return false;
      }
      pos = static_cast<size_t>(next);
    } else {
      // Last OBU in the temporal unit: it runs to the next temporal delimiter
      // or to the end of the stream.  Without a size we must stop here.
      payload = buffer.size() - pos;
    }
    if (payload > buffer.size() - pos) {
      diagnostics->push_back(make(ErrorCode::internal, "AV1 OBU size exceeds the stream",
                                  {.byte_offset = static_cast<int64_t>(header)}));
      return false;
    }
    const size_t unit_length = (pos - header) + static_cast<size_t>(payload);

    if (type == 2) {  // OBU_TEMPORAL_DELIMITER
      fresh_temporal_unit = true;
      temporal_unit_start = static_cast<int64_t>(header);
    } else if (av1_obu_is_frame(type) && fresh_temporal_unit) {
      // A frame with a shown frame header carries frame_type in the first
      // bits after the uncompressed header; without a full parse we rely on
      // the fixture contract that the first frame of every temporal unit is
      // a keyframe only when its header marks it so.
      const size_t start = pos;
      const size_t end = std::min(buffer.size(), start + static_cast<size_t>(payload));
      bool key = false;
      if (start + 1 < end) {
        // frame_type is bit 5 of the first payload byte for a shown-frame
        // OBU_FRAME starting with a frame header.
        const uint8_t header_byte = static_cast<uint8_t>(buffer[start]);
        // bit 7 is show_existing_frame; frame_type 0 (KEY_FRAME) is bits 6-5.
        key = ((header_byte >> 5) & 0x03) == 0;
      }
      if (key) {
        StreamUnit unit;
        // The index records the start of the temporal unit (its temporal
        // delimiter / sequence header OBU), matching encoder packet offsets.
        unit.offset = temporal_unit_start;
        unit.au_offset = temporal_unit_start;
        unit.starts_au = true;
        unit.length = static_cast<int64_t>(unit_length);
        unit.nal_type = type;
        unit.keyframe = true;
        out->units.push_back(unit);
        out->keyframes.push_back(unit);
      }
      fresh_temporal_unit = false;
    }
    pos += static_cast<size_t>(payload);
    if (out->units.size() > limits.max_index_entries) {
      diagnostics->push_back(make(ErrorCode::index_too_large,
                                  "raw stream has more keyframes than the limit",
                                  {}));
      return false;
    }
  }
  out->codec = Codec::av1;
  out->stream_size = size;
  out->unit_count = out->units.size();
  return true;
}

}  // namespace

bool sniff_codec(std::string_view prefix, Codec* codec) {
  // Locate the first Annex B start code; encoders may emit 3 or 4 bytes.
  for (size_t i = 0; i + 3 < prefix.size(); ++i) {
    if (static_cast<uint8_t>(prefix[i]) != 0 ||
        static_cast<uint8_t>(prefix[i + 1]) != 0 ||
        static_cast<uint8_t>(prefix[i + 2]) != 1) {
      continue;
    }
    const uint8_t header = static_cast<uint8_t>(prefix[i + 3]);
    const int h264_type = header & 0x1F;
    const int h265_type = (header >> 1) & 0x3F;
    // VPS/SPS/PPS/prefix SEI are unambiguous HEVC markers; their bytes read
    // as invalid or reserved H.264 types.  H.265 VCL types 0..9 overlap with
    // H.264 slice types, so the parameter-set check comes first.
    if (h265_type == 32 || h265_type == 33 || h265_type == 34 ||
        h265_type == 39 || h265_type == 40) {
      *codec = Codec::h265;
      return true;
    }
    if (h264_type == 1 || h264_type == 5 || h264_type == 6 ||
        h264_type == 7 || h264_type == 8 || h264_type == 9) {
      *codec = Codec::h264;
      return true;
    }
    if ((h265_type >= 16 && h265_type <= 21) ||
        (h265_type >= 0 && h265_type <= 9)) {
      *codec = Codec::h265;
      return true;
    }
    return false;
  }
  // AV1 low-overhead OBU: temporal delimiter (0x12) or sequence header
  // (0x0A) as the first byte; the forbidden bit must be clear.
  if (!prefix.empty()) {
    const uint8_t byte = static_cast<uint8_t>(prefix[0]);
    const int type = (byte >> 3) & 0x0F;
    if ((byte & 0x80) == 0 && (type == 2 || type == 1)) {
      *codec = Codec::av1;
      return true;
    }
  }
  return false;
}

bool scan_raw_stream(const std::filesystem::path& path, Codec codec,
                     const Limits& limits, StreamScan* out,
                     std::vector<Diagnostic>* diagnostics) {
  int64_t size = 0;
  if (!file_size(path, &size, nullptr)) {
    diagnostics->push_back(make(ErrorCode::io, "cannot stat raw stream",
                                {.file = path.string()}));
    return false;
  }
  if (codec == Codec::av1) {
    if (size > limits.max_media_bytes) {
      diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                  "raw stream exceeds the media size limit",
                                  {.file = path.string()}));
      return false;
    }
    std::string buffer(static_cast<size_t>(size), '\0');
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
      diagnostics->push_back(make(ErrorCode::io, "cannot open raw stream",
                                  {.file = path.string()}));
      return false;
    }
    if (size > 0) {
      stream.read(buffer.data(), static_cast<std::streamsize>(size));
      if (!stream) {
        diagnostics->push_back(make(ErrorCode::io, "short read",
                                    {.file = path.string()}));
        return false;
      }
    }
    return scan_av1(buffer, limits, out, diagnostics);
  }
  std::ifstream stream(path, std::ios::binary);
  if (!stream) {
    diagnostics->push_back(make(ErrorCode::io, "cannot open raw stream",
                                {.file = path.string()}));
    return false;
  }
  return scan_annexb(stream, size, codec, limits, out, diagnostics);
}

bool scan_raw_stream_bytes(std::string_view bytes, Codec codec,
                           const Limits& limits, StreamScan* out,
                           std::vector<Diagnostic>* diagnostics) {
  if (codec == Codec::av1) {
    return scan_av1(std::string(bytes), limits, out, diagnostics);
  }
  std::istringstream stream{std::string(bytes)};
  return scan_annexb(stream, static_cast<int64_t>(bytes.size()), codec, limits,
                     out, diagnostics);
}

const char* entry_origin_name(EntryOrigin origin) {
  return origin == EntryOrigin::index ? "index" : "scan";
}

bool load_keyframe_index(const std::filesystem::path& path, const Limits& limits,
                         KeyframeIndex* out,
                         std::vector<Diagnostic>* diagnostics) {
  std::string text;
  Diagnostic error;
  if (!read_file(path, limits.max_manifest_bytes, &text, &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  std::string parse_error;
  size_t offset = 0;
  const Json root = Json::parse(text, &parse_error, &offset);
  if (!parse_error.empty()) {
    diagnostics->push_back(make(ErrorCode::json_parse,
                                "keyframe index: " + parse_error,
                                {.byte_offset = static_cast<int64_t>(offset)}));
    return false;
  }
  if (!root.is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "keyframe index root must be an object",
                                {.file = path.string()}));
    return false;
  }
  const Json* format = root.find("format");
  if (format == nullptr || !format->is_string() ||
      format->as_string() != "media-archive-keyframe-index") {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "unexpected keyframe index format",
                                {.file = path.string(), .field = "format"}));
    return false;
  }
  const Json* version = root.find("version");
  if (version == nullptr || !version->is_integer()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "keyframe index needs an integer version",
                                {.file = path.string(), .field = "version"}));
    return false;
  }
  if (version->as_integer() < kIndexVersionMin ||
      version->as_integer() > kIndexVersionMax) {
    diagnostics->push_back(make(ErrorCode::index_version,
                                "unsupported keyframe index version " +
                                    std::to_string(version->as_integer()),
                                {.file = path.string(), .field = "version"}));
    return false;
  }
  out->present = true;
  out->path = path.string();
  out->version = static_cast<int>(version->as_integer());
  const Json* entries = root.find("entries");
  if (entries == nullptr || !entries->is_array()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "keyframe index needs an entries array",
                                {.file = path.string(), .field = "entries"}));
    return false;
  }
  if (entries->items().size() > limits.max_index_entries) {
    diagnostics->push_back(make(ErrorCode::index_too_large,
                                "keyframe index exceeds the entry limit",
                                {.file = path.string()}));
    return false;
  }
  int64_t previous_tick = -1;
  for (const Json& item : entries->items()) {
    if (!item.is_object()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "index entry must be an object", {.file = path.string()}));
      return false;
    }
    const Json* tick = item.find("tick");
    const Json* entry_offset = item.find("offset");
    if (tick == nullptr || !tick->is_integer() || entry_offset == nullptr ||
        !entry_offset->is_integer()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "index entry needs integer tick and offset",
                                  {.file = path.string()}));
      return false;
    }
    const Json* keyframe = item.find("keyframe");
    const bool is_keyframe =
        keyframe == nullptr ? true
                            : (keyframe->is_bool() ? keyframe->as_bool()
                                                   : keyframe->is_integer() &&
                                                         keyframe->as_integer() != 0);
    if (entry_offset->as_integer() < 0) {
      diagnostics->push_back(make(ErrorCode::index_negative_offset,
                                  "keyframe offsets must not be negative",
                                  {.file = path.string(),
                                   .byte_offset = entry_offset->as_integer()}));
      return false;
    }
    if (tick->as_integer() <= previous_tick) {
      diagnostics->push_back(make(ErrorCode::index_order,
                                  "keyframe ticks must strictly increase",
                                  {.file = path.string(), .tick = tick->as_integer()}));
      return false;
    }
    previous_tick = tick->as_integer();
    for (const auto& existing : out->entries) {
      if (existing.offset == entry_offset->as_integer()) {
        diagnostics->push_back(make(ErrorCode::index_duplicate,
                                    "duplicate keyframe offset",
                                    {.file = path.string(),
                                     .byte_offset = entry_offset->as_integer()}));
        return false;
      }
    }
    if (tick->as_integer() < 0) {
      diagnostics->push_back(make(ErrorCode::index_negative_offset,
                                  "keyframe ticks must not be negative",
                                  {.file = path.string(), .tick = tick->as_integer()}));
      return false;
    }
    out->entries.push_back({tick->as_integer(), entry_offset->as_integer(), is_keyframe});
  }
  return true;
}

bool validate_keyframe_index(const KeyframeIndex& index, const StreamScan& scan,
                             const Limits& limits, IndexReport* report,
                             std::vector<Diagnostic>* diagnostics) {
  const size_t before = diagnostics->size();
  report->index_entries = index.entries.size();
  report->scan_keyframes = scan.keyframes.size();
  if (index.entries.size() > limits.max_index_entries) {
    diagnostics->push_back(make(ErrorCode::index_too_large,
                                "keyframe index exceeds the entry limit", {}));
    return false;
  }

  // Map scanned keyframe offsets to units for fast membership tests.
  std::map<int64_t, const StreamUnit*> by_offset;
  for (const auto& unit : scan.keyframes) by_offset[unit.offset] = &unit;

  std::set<int64_t> seen_offsets;
  std::set<int64_t> seen_ticks;
  int64_t previous_offset = -1;
  int64_t previous_tick = -1;
  for (size_t i = 0; i < index.entries.size(); ++i) {
    const IndexEntry& entry = index.entries[i];
    Location loc;
    loc.byte_offset = entry.offset;
    loc.tick = entry.tick;
    if (!entry.keyframe) {
      diagnostics->push_back(make(ErrorCode::index_not_keyframe,
                                  "index entry is marked as a non-keyframe", loc));
    }
    if (entry.offset < 0) {
      diagnostics->push_back(make(ErrorCode::index_negative_offset,
                                  "keyframe offset is negative", loc));
    } else if (entry.offset >= scan.stream_size) {
      diagnostics->push_back(make(ErrorCode::index_out_of_range,
                                  "keyframe offset is past the end of the stream", loc));
    }
    if (entry.tick < 0) {
      diagnostics->push_back(make(ErrorCode::index_negative_offset,
                                  "keyframe tick is negative", loc));
    }
    if (!seen_offsets.insert(entry.offset).second) {
      diagnostics->push_back(make(ErrorCode::index_duplicate,
                                  "duplicate keyframe offset", loc));
    }
    if (!seen_ticks.insert(entry.tick).second) {
      diagnostics->push_back(make(ErrorCode::index_duplicate,
                                  "duplicate keyframe tick", loc));
    }
    if (entry.offset <= previous_offset) {
      diagnostics->push_back(make(ErrorCode::index_order,
                                  "keyframe offsets must strictly increase", loc));
    }
    if (entry.tick <= previous_tick) {
      diagnostics->push_back(make(ErrorCode::index_order,
                                  "keyframe ticks must strictly increase", loc));
    }
    previous_offset = entry.offset;
    previous_tick = entry.tick;
    if (entry.offset >= 0 && entry.offset < scan.stream_size &&
        !by_offset.contains(entry.offset)) {
      diagnostics->push_back(make(ErrorCode::index_not_keyframe,
                                  "index entry does not point at a scanned keyframe", loc));
    }
    if (by_offset.contains(entry.offset)) {
      ++report->hits;
    }
    KeyframeCandidate candidate;
    candidate.offset = entry.offset;
    candidate.tick = entry.tick;
    candidate.origin = EntryOrigin::index;
    report->candidates.push_back(candidate);
  }

  // Fill missing keyframes from the scan.  Candidates keep their scanned
  // origin so callers can tell recovered entries from declared ones.
  for (const auto& unit : scan.keyframes) {
    if (seen_offsets.contains(unit.offset)) continue;
    KeyframeCandidate candidate;
    candidate.offset = unit.offset;
    candidate.origin = EntryOrigin::scan;
    report->candidates.push_back(candidate);
    ++report->generated;
  }
  std::sort(report->candidates.begin(), report->candidates.end(),
            [](const KeyframeCandidate& a, const KeyframeCandidate& b) {
              return a.offset < b.offset;
            });
  return diagnostics->size() == before;
}

}  // namespace media_restore
