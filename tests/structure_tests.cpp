#include <catch2/catch_test_macros.hpp>

#include <cstdio>
#include <filesystem>
#include <fstream>
#include <string>
#include <vector>

#include "mp4_structure.hpp"

using namespace mediaqc;

namespace {

std::string tmp_path(const char* name) {
  auto p = std::filesystem::temp_directory_path() / name;
  return p.string();
}

void write_bytes(const std::string& path, const std::string& data) {
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  out.write(data.data(), static_cast<std::streamsize>(data.size()));
}

// Minimal synthetic MP4: ftyp, moov with one trak (stbl: stco/stsz/stsc/stts),
// then mdat holding the samples.  Sample layout is fully controlled so we can
// assert exact offsets.
struct Sample {
  std::string bytes;
};

std::string u32(uint32_t v) {
  std::string s(4, '\0');
  s[0] = char(v >> 24); s[1] = char(v >> 16); s[2] = char(v >> 8); s[3] = char(v);
  return s;
}

std::string box(const std::string& type, const std::string& payload) {
  return u32(uint32_t(8 + payload.size())) + type + payload;
}

std::string make_mp4(const std::vector<Sample>& samples, bool moov_last,
                     uint32_t timescale = 1000) {
  const std::string ftyp = box("ftyp", std::string("isom\0\0\2\0isom", 12));
  std::string mdat_payload;
  for (const auto& s : samples) mdat_payload += s.bytes;

  // Two passes: the moov size depends on the stco entries, whose values
  // depend on where mdat starts, which depends on the moov size.
  auto build_moov = [&](int64_t mdat_payload_start) {
    std::string stco = std::string(4, '\0') + u32(uint32_t(samples.size()));
    int64_t off = mdat_payload_start;
    for (const auto& s : samples) {
      stco += u32(uint32_t(off));
      off += int64_t(s.bytes.size());
    }
    std::string stsz = std::string(4, '\0') + u32(0) + u32(uint32_t(samples.size()));
    for (const auto& s : samples) stsz += u32(uint32_t(s.bytes.size()));
    std::string stsc = std::string(4, '\0') + u32(1) + u32(1) +
                       u32(uint32_t(samples.size())) + u32(1);
    std::string stts = std::string(4, '\0') + u32(1) + u32(uint32_t(samples.size())) + u32(40);
    const std::string stsd = box("stsd", std::string(4, '\0') + u32(0));
    const std::string stbl = box("stbl", stsd + box("stsc", stsc) + box("stsz", stsz) +
                                              box("stco", stco) + box("stts", stts));
    std::string mdhd_payload(4, '\0');
    mdhd_payload += u32(0) + u32(0) + u32(timescale) + u32(0) + u32(0) + u32(0);
    std::string hdlr_payload(4, '\0');
    hdlr_payload += u32(0) + std::string("vide", 4) + std::string(12, '\0');
    std::string tkhd_payload(4, '\0');
    tkhd_payload += u32(0) + u32(0) + u32(1) + u32(0) + std::string(60, '\0');
    const std::string trak = box(
        "trak", box("tkhd", tkhd_payload) +
                    box("mdia", box("mdhd", mdhd_payload) + box("hdlr", hdlr_payload) +
                                    box("minf", box("stbl", stbl))));
    return box("moov", trak);
  };
  const std::string mdat_header = u32(uint32_t(8 + mdat_payload.size())) + "mdat";
  if (moov_last) {
    const int64_t payload_start = int64_t(ftyp.size()) + 8 + 8;
    return ftyp + mdat_header + mdat_payload + build_moov(payload_start);
  }
  // Pass 1 with a placeholder to learn the moov size, pass 2 with real
  // offsets.  stco values fit in 32 bits, so the size is stable.
  const std::string probe = build_moov(0);
  const int64_t payload_start = int64_t(ftyp.size() + probe.size()) + 8;
  const std::string moov = build_moov(payload_start);
  return ftyp + moov + mdat_header + mdat_payload;
}

}  // namespace

TEST_CASE("structure walker reads top-level boxes") {
  const std::string ftyp = box("ftyp", "isom");
  const std::string mdat = box("mdat", "abcd");
  std::vector<Box> boxes;
  const std::string data = ftyp + mdat;
  walk_boxes(reinterpret_cast<const uint8_t*>(data.data()), int64_t(data.size()), 0, 0, boxes);
  REQUIRE(boxes.size() == 2);
  REQUIRE(boxes[0].type_str() == "ftyp");
  REQUIRE(boxes[1].type_str() == "mdat");
  REQUIRE(boxes[1].offset == int64_t(ftyp.size()));
  REQUIRE(boxes[1].size == 12);
}

TEST_CASE("structure walker flags a box that runs past the buffer") {
  std::string data = u32(100) + "mdat" + "1234";
  std::vector<Box> boxes;
  walk_boxes(reinterpret_cast<const uint8_t*>(data.data()), int64_t(data.size()), 0, 0, boxes);
  REQUIRE(boxes.size() == 1);
  REQUIRE(boxes[0].truncated);
  REQUIRE(boxes[0].size == int64_t(data.size()));
}

TEST_CASE("sample offsets are reconstructed from stco/stsc/stsz") {
  const std::vector<Sample> samples{{"aaaa"}, {"bbbbbb"}, {"cc"}};
  const std::string path = tmp_path("mediaqc_struct_ok.mp4");
  write_bytes(path, make_mp4(samples, false));
  FileStructure fs;
  Error err;
  REQUIRE(read_mp4_structure(path, fs, err));
  REQUIRE(fs.has_moov);
  REQUIRE_FALSE(fs.moov_after_mdat);
  REQUIRE_FALSE(fs.truncated);
  REQUIRE(fs.tracks.size() == 1);
  const auto& t = fs.tracks[0];
  REQUIRE(t.track_id == 1);
  REQUIRE(t.is_video());
  REQUIRE(t.timescale == 1000);
  REQUIRE(t.samples.size() == 3);
  REQUIRE(t.sample_sizes[1] == 6);
  REQUIRE(t.usable_index());
  REQUIRE(t.samples[0].offset == fs.mdats[0].data_offset);
  REQUIRE(t.samples[2].offset == fs.mdats[0].data_offset + 10);
  REQUIRE(t.sample_dts.size() == 3);
  REQUIRE(t.sample_dts[2] == 80);
  std::filesystem::remove(path);
}

TEST_CASE("moov after mdat is detected") {
  const std::vector<Sample> samples{{"aaaa"}};
  const std::string path = tmp_path("mediaqc_struct_moovlast.mp4");
  write_bytes(path, make_mp4(samples, true));
  FileStructure fs;
  Error err;
  REQUIRE(read_mp4_structure(path, fs, err));
  REQUIRE(fs.has_moov);
  REQUIRE(fs.moov_after_mdat);
  std::filesystem::remove(path);
}

TEST_CASE("truncated mdat is localised to the first missing byte") {
  std::vector<Sample> samples{{"aaaa"}, {"bbbbbb"}, {"cc"}};
  std::string full = make_mp4(samples, false);
  const std::string path = tmp_path("mediaqc_struct_trunc.mp4");
  // Cut two bytes off the final sample.
  write_bytes(path, full.substr(0, full.size() - 2));
  FileStructure fs;
  Error err;
  REQUIRE(read_mp4_structure(path, fs, err));
  REQUIRE(fs.truncated);
  REQUIRE(fs.last_confirmed_offset == int64_t(full.size() - 2));
  REQUIRE(fs.tracks.size() == 1);
  const auto& t = fs.tracks[0];
  REQUIRE(t.first_bad_sample == 2);
  REQUIRE(t.first_bad_offset == fs.mdats[0].data_offset + 10);
  REQUIRE_FALSE(t.usable_index());
  std::filesystem::remove(path);
}

TEST_CASE("out-of-range chunk offset is reported") {
  const std::vector<Sample> samples{{"aaaa"}, {"bbbbbb"}};
  std::string data = make_mp4(samples, false);
  // Patch the first stco entry to point past EOF.
  const size_t pos = data.find("stco");
  REQUIRE(pos != std::string::npos);
  const uint32_t huge = 0x7FFFFFF0u;
  data[pos + 12] = char(huge >> 24); data[pos + 13] = char(huge >> 16);
  data[pos + 14] = char(huge >> 8); data[pos + 15] = char(huge);
  const std::string path = tmp_path("mediaqc_struct_badstco.mp4");
  write_bytes(path, data);
  FileStructure fs;
  Error err;
  REQUIRE(read_mp4_structure(path, fs, err));
  REQUIRE(fs.tracks.size() == 1);
  const auto& t = fs.tracks[0];
  REQUIRE(t.index_entries_out_of_range >= 1);
  REQUIRE(t.first_bad_sample == 0);
  std::filesystem::remove(path);
}

TEST_CASE("file without moov reports no moov") {
  std::string data = box("ftyp", "isom") + box("mdat", "abcdefgh");
  const std::string path = tmp_path("mediaqc_struct_nomoov.mp4");
  write_bytes(path, data);
  FileStructure fs;
  Error err;
  REQUIRE(read_mp4_structure(path, fs, err));
  REQUIRE_FALSE(fs.has_moov);
  std::filesystem::remove(path);
}
