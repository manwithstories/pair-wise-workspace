#pragma once
// Shared deterministic helpers for Catch2 tests.
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <string>
#include <unistd.h>

#include "media/fs_util.hpp"
#include "media/json.hpp"

namespace mediaqc_test {

inline std::string wav_bytes(int rate, int channels, int bps, int frames,
                             uint32_t seed = 12785) {
    const int block = channels * bps / 8;
    const int64_t data_bytes = static_cast<int64_t>(frames) * block;
    std::string s;
    auto u16 = [&](uint16_t v) { s.push_back(v & 0xFF); s.push_back((v >> 8) & 0xFF); };
    auto u32 = [&](uint32_t v) { for (int i = 0; i < 4; ++i) s.push_back((v >> (8 * i)) & 0xFF); };
    s.append("RIFF", 4);
    u32(static_cast<uint32_t>(36 + data_bytes));
    s.append("WAVEfmt ", 8);
    u32(16);
    u16(1);
    u16(static_cast<uint16_t>(channels));
    u32(rate);
    u32(static_cast<uint32_t>(rate * block));
    u16(static_cast<uint16_t>(block));
    u16(static_cast<uint16_t>(bps));
    s.append("data", 4);
    u32(static_cast<uint32_t>(data_bytes));
    for (int f = 0; f < frames; ++f)
        for (int c = 0; c < channels; ++c)
            u16(static_cast<uint16_t>((seed * 1103515245u + f * 31 + c * 7) & 0x7FFF));
    return s;
}

inline std::string tmp_dir() {
    static int counter = 0;
    const auto p = std::filesystem::temp_directory_path() /
                   ("mediaqc-test-" + std::to_string(::getpid()) + "-" +
                    std::to_string(++counter));
    std::filesystem::create_directories(p);
    return std::filesystem::canonical(p).string();
}

// Index sidecar with 4800-frame (0.1 s), six-channel s16 pages.
inline void write_index_sidecar(const std::string& wav_path, int frames_per_page = 4800) {
    constexpr int header_bytes = 44;
    constexpr int block = 12;
    const auto st = mediaqc::fs::stat(wav_path);
    const std::string bytes = mediaqc::fs::read_file(wav_path);
    mediaqc::json::Array pages;
    int64_t off = header_bytes;
    int64_t sample = 0;
    int no = 0;
    while (off < st.size) {
        const int64_t cap = static_cast<int64_t>(frames_per_page) * block;
        const int64_t frames = std::min<int64_t>(cap, st.size - off) / block;
        if (frames == 0) break;
        const uint32_t crc = mediaqc::fs::crc32_ieee(
            reinterpret_cast<const unsigned char*>(bytes.data()) + off,
            static_cast<size_t>(frames * block));
        pages.emplace_back(mediaqc::json::Object{
            {"page_no", no}, {"offset", off}, {"size", frames * block},
            {"start_sample", sample}, {"end_sample", sample + frames},
            {"crc32", static_cast<int64_t>(crc)},
        });
        off += frames * block;
        sample += frames;
        ++no;
    }
    mediaqc::json::Value doc(
        mediaqc::json::Object{{"pages", mediaqc::json::Value(std::move(pages))}});
    mediaqc::fs::write_atomic(wav_path + ".idx.json", mediaqc::json::dump(doc));
}

}  // namespace mediaqc_test
