#include "media/stream_reader.hpp"
#include "media/checksum.hpp"

namespace media {

namespace {

// ---- H.264 / H.265 Annex B scanner -------------------------------------------
// Splits on 00 00 01 / 00 00 00 01 start codes, reads the NAL type from the
// syntactically valid header produced by sample_generator and groups VCL NALs
// into frames. Parameter sets are configuration, not frames.
struct AnnexBScanner {
  std::vector<Frame> frames;
  bool truncated = false;
};

std::size_t find_start_code(const std::vector<std::uint8_t>& b,
                            std::size_t from, int& prefix_len) {
  for (std::size_t i = from; i + 3 <= b.size(); ++i) {
    if (b[i] == 0 && b[i + 1] == 0) {
      if (b[i + 2] == 1) { prefix_len = 3; return i; }
      if (i + 3 < b.size() && b[i + 2] == 0 && b[i + 3] == 1) {
        prefix_len = 4; return i;
      }
    }
  }
  prefix_len = 0;
  return b.size();
}

std::uint8_t nal_type(std::uint8_t hdr, bool hevc) {
  return hevc ? static_cast<std::uint8_t>((hdr >> 1) & 0x3f)
              : static_cast<std::uint8_t>(hdr & 0x1f);
}

bool is_vcl_nal(std::uint8_t type, bool hevc) {
  // H.265 VCL NALs: non-IRAP 0..9 (excluding reserved 8/9 in practice) and
  // IRAP 16..23; parameter sets occupy 32+. H.264 VCL is slice types 1/5.
  if (!hevc) return type == 1 || type == 5;
  return type <= 7 || (type >= 16 && type <= 23);
}

AnnexBScanner scan_annexb(const std::vector<std::uint8_t>& bytes,
                          CodecType codec, std::int64_t frame_dur_us) {
  AnnexBScanner r;
  const bool hevc = codec == CodecType::H265;
  std::size_t pos = 0;
  std::uint32_t idx = 0;
  while (pos < bytes.size()) {
    int plen = 0;
    const std::size_t sc = find_start_code(bytes, pos, plen);
    if (plen == 0) {
      // Trailing bytes with no further start code: truncated only when real
      // NAL payload bytes remain after the last complete unit.
      if (sc > pos) r.truncated = true;
      break;
    }
    const std::size_t hdr_pos = sc + static_cast<std::size_t>(plen);
    if (hdr_pos >= bytes.size()) { r.truncated = true; break; }
    int next_plen = 0;
    const std::size_t next_sc = find_start_code(bytes, hdr_pos, next_plen);
    const bool last_unit = (next_plen == 0);
    // Truncation: after the last start code there are leftover bytes but not
    // enough for even a NAL header, i.e. a unit cut off mid-flight.
    if (last_unit && bytes.size() < hdr_pos + 2) r.truncated = true;
    const std::uint8_t type = nal_type(bytes[hdr_pos], hevc);
    if (is_vcl_nal(type, hevc)) {
      const bool key = hevc ? (type == 19 || type == 20) : (type == 5);
      Frame f;
      f.codec = codec;
      f.index = idx++;
      f.pts_us = f.dts_us = static_cast<std::int64_t>(f.index) * frame_dur_us;
      f.keyframe = key;
      f.offset = sc;
      const std::size_t unit_end =
          last_unit ? bytes.size() : next_sc;
      f.data.assign(bytes.begin() + static_cast<std::ptrdiff_t>(sc),
                    bytes.begin() + static_cast<std::ptrdiff_t>(unit_end));
      r.frames.push_back(std::move(f));
    }
    pos = last_unit ? bytes.size() : next_sc;
  }
  return r;
}

// ---- AV1 OBU scanner ---------------------------------------------------------
// Temporal unit layout produced by sample_generator:
//   u32 length, then OBUs; OBU header type in bits 6..3, our key marker is
//   0x80 on the first payload byte. Sequence header (type 1) makes the next
//   frame unit a key frame.
struct Av1Scan {
  std::vector<Frame> frames;
  bool truncated = false;
};

Av1Scan scan_av1(const std::vector<std::uint8_t>& bytes,
                 std::int64_t frame_dur_us) {
  Av1Scan r;
  std::size_t p = 0;
  std::uint32_t idx = 0;
  bool pending_key = false;
  while (p + 4 <= bytes.size()) {
    std::uint32_t len = (std::uint32_t(bytes[p]) << 24) |
                        (std::uint32_t(bytes[p + 1]) << 16) |
                        (std::uint32_t(bytes[p + 2]) << 8) |
                        std::uint32_t(bytes[p + 3]);
    p += 4;
    if (len == 0 || p + len > bytes.size()) { r.truncated = true; break; }
    std::size_t q = p;
    bool is_frame = false, key = false;
    while (q + 2 <= p + len) {
      const std::uint8_t hdr = bytes[q++];
      const std::uint8_t obu_len = bytes[q++];
      const std::uint8_t type = (hdr >> 3) & 0x0f;
      if (q + obu_len > p + len) { r.truncated = true; break; }
      if (type == 1) pending_key = true;                 // sequence header
      if (type == 3) {
        is_frame = true;
        key = (bytes[q] & 0x80) != 0 || pending_key;
        pending_key = false;
      }
      q += obu_len;
    }
    if (is_frame) {
      Frame f;
      f.codec = CodecType::AV1;
      f.index = idx++;
      f.pts_us = f.dts_us = static_cast<std::int64_t>(f.index) * frame_dur_us;
      f.keyframe = key;
      f.offset = p - 4;
      f.data.assign(bytes.begin() + static_cast<std::ptrdiff_t>(f.offset),
                    bytes.begin() + static_cast<std::ptrdiff_t>(p + len));
      r.frames.push_back(std::move(f));
    }
    p += len;
  }
  if (p != bytes.size()) r.truncated = true;
  return r;
}

} // namespace

// Minimal index handling for the parser batch; full validation/rebuild lands
// in the next batch (stream_index.cpp).
static void validate_index(ReadResult& r,
                           const std::vector<std::uint8_t>& bytes,
                           const StreamIndex* supplied) {
  if (supplied) {
    r.validated_index = *supplied;
    return;
  }
  r.validated_index.declared_size = bytes.size();
  for (const auto& f : r.frames)
    if (f.keyframe)
      r.validated_index.entries.push_back({f.index, f.offset, f.pts_us});
}

ReadResult read_stream(const std::vector<std::uint8_t>& bytes,
                       CodecType declared,
                       const StreamIndex* supplied_index,
                       std::uint64_t expected_size) {
  ReadResult r;
  r.input_hash = fnv1a64(bytes.data(), bytes.size());
  constexpr std::int64_t kFrameDurUs = 33'333;

  const CodecType probed = probe_codec(bytes);
  if (probed != CodecType::Unknown && probed != declared)
    r.errors.push_back({ErrorCode::StreamTruncated, 0, 0,
                        std::string("codec probe=") + codec_name(probed) +
                        " declared=" + codec_name(declared)});

  if (declared == CodecType::AV1) {
    Av1Scan s = scan_av1(bytes, kFrameDurUs);
    r.frames = std::move(s.frames);
    r.truncated = s.truncated;
  } else {
    AnnexBScanner s = scan_annexb(bytes, declared, kFrameDurUs);
    r.frames = std::move(s.frames);
    r.truncated = s.truncated;
  }
  if (expected_size > bytes.size()) r.truncated = true;
  if (r.truncated)
    r.errors.push_back({ErrorCode::StreamTruncated, 0,
                        r.frames.size(), "stream ends inside a frame"});
  r.validated_index.codec = declared;
  validate_index(r, bytes, supplied_index);
  return r;
}

} // namespace media
