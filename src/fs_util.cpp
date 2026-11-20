#include "media/fs_util.hpp"

#include <algorithm>
#include <array>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <unistd.h>
#include <sys/stat.h>

namespace mediaqc::fs {

namespace stdfs = std::filesystem;

std::string safe_join(const std::string& root, const std::string& relative) {
    if (relative.empty()) throw std::runtime_error("empty relative path");
    if (relative.front() == '/') throw std::runtime_error("absolute path rejected: " + relative);
    stdfs::path base;
    try {
        base = stdfs::weakly_canonical(root);
    } catch (const std::exception& e) {
        throw std::runtime_error(std::string("cannot resolve root: ") + e.what());
    }
    stdfs::path joined = base;
    for (const auto& part : stdfs::path(relative)) {
        if (part == "..")
            throw std::runtime_error("parent traversal rejected: " + relative);
        if (part == ".") continue;
        if (part.native().find('\0') != std::string::npos)
            throw std::runtime_error("nul byte in path");
        joined /= part;
    }
    const stdfs::path canonical = stdfs::exists(joined) ? stdfs::canonical(joined)
                                                        : stdfs::weakly_canonical(joined);
    const std::string c = canonical.string();
    const std::string b = base.string();
    if (c != b && c.compare(0, b.size(), b) != 0)
        throw std::runtime_error("path escapes root: " + relative);
    if (c != b && c.size() > b.size() && c[b.size()] != '/')
        throw std::runtime_error("path escapes root (prefix): " + relative);
    return c;
}

void ensure_dir(const std::string& path) {
    stdfs::path p(path);
    if (p.empty()) return;
    stdfs::create_directories(p);
    std::error_code ec;
    auto perms = stdfs::status(p, ec).permissions();
    if (ec) throw std::runtime_error("cannot stat directory: " + path);
    (void)perms;
}

std::string validate_output_dir(const std::string& path) {
    stdfs::path p = stdfs::weakly_canonical(path);
    if (!stdfs::exists(p)) stdfs::create_directories(p);
    if (!stdfs::is_directory(p))
        throw std::runtime_error("output path is not a directory: " + path);
    const std::string probe = (p / ".mediaqc.write-probe").string();
    { std::ofstream f(probe, std::ios::binary); if (!f) throw std::runtime_error("output dir not writable: " + path); }
    stdfs::remove(probe);
    return p.string();
}

void write_atomic(const std::string& final_path, const std::string& bytes) {
    const std::string tmp = final_path + ".tmp." + std::to_string(static_cast<long long>(::getpid()));
    {
        std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
        if (!f) throw std::runtime_error("cannot open temp file: " + tmp);
        f.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
        if (!f) throw std::runtime_error("write failed: " + tmp);
    }
    std::error_code ec;
    stdfs::rename(tmp, final_path, ec);
    if (ec) {
        stdfs::remove(tmp, ec);
        throw std::runtime_error("atomic rename failed for " + final_path + ": " + ec.message());
    }
}

std::string read_file(const std::string& path, size_t cap_bytes) {
    std::ifstream f(path, std::ios::binary);
    if (!f) throw std::runtime_error("cannot open: " + path);
    std::string out;
    std::array<char, 65536> buf{};
    while (f) {
        f.read(buf.data(), static_cast<std::streamsize>(buf.size()));
        const auto n = f.gcount();
        if (n > 0) {
            if (out.size() + static_cast<size_t>(n) > cap_bytes)
                throw std::runtime_error("file exceeds size cap: " + path);
            out.append(buf.data(), static_cast<size_t>(n));
        }
    }
    return out;
}

void read_chunks(const std::string& path, ChunkFn fn, void* user) {
    std::ifstream f(path, std::ios::binary);
    if (!f) throw std::runtime_error("cannot open: " + path);
    std::array<unsigned char, 1 << 20> buf{};
    while (f) {
        f.read(reinterpret_cast<char*>(buf.data()), static_cast<std::streamsize>(buf.size()));
        const auto n = f.gcount();
        if (n > 0) fn(buf.data(), static_cast<size_t>(n), user);
    }
}

StatInfo stat(const std::string& path) {
    struct ::stat st {};
    if (::stat(path.c_str(), &st) != 0) return {};
    StatInfo info;
    info.exists = true;
    info.size = static_cast<int64_t>(st.st_size);
#if defined(__APPLE__)
    info.mtime_ns = static_cast<int64_t>(st.st_mtimespec.tv_sec) * 1'000'000'000LL + st.st_mtimespec.tv_nsec;
#else
    info.mtime_ns = static_cast<int64_t>(st.st_mtim.tv_sec) * 1'000'000'000LL + st.st_mtim.tv_nsec;
#endif
    return info;
}

std::vector<std::string> list_files(const std::string& dir) {
    std::vector<std::string> names;
    if (!stdfs::is_directory(dir)) return names;
    for (const auto& e : stdfs::directory_iterator(dir))
        if (e.is_regular_file()) names.push_back(e.path().filename().string());
    std::sort(names.begin(), names.end());
    return names;
}

namespace {

// Slice-by-8 IEEE CRC-32 table; built once, gives roughly an order of
// magnitude more throughput than the bitwise loop on large PCM ranges.
struct CrcTable {
    uint32_t slice[8][256];
    CrcTable() {
        for (uint32_t n = 0; n < 256; ++n) {
            uint32_t c = n;
            for (int k = 0; k < 8; ++k)
                c = (c & 1) ? (0xEDB88320u ^ (c >> 1)) : (c >> 1);
            slice[0][n] = c;
        }
        for (uint32_t n = 0; n < 256; ++n) {
            uint32_t c = slice[0][n];
            for (int k = 1; k < 8; ++k) {
                c = slice[0][c & 0xFF] ^ (c >> 8);
                slice[k][n] = c;
            }
        }
    }
};

const CrcTable& crc_table() {
    static const CrcTable table;
    return table;
}

}  // namespace

uint32_t crc32_ieee(const unsigned char* data, size_t size, uint32_t crc) {
    const auto& t = crc_table();
    crc = ~crc;
    // Align to an 8-byte boundary first.
    while (size > 0 && (reinterpret_cast<uintptr_t>(data) & 7) != 0) {
        crc = t.slice[0][(crc ^ *data) & 0xFF] ^ (crc >> 8);
        ++data;
        --size;
    }
    while (size >= 8) {
        uint64_t word;
        std::memcpy(&word, data, sizeof(word));
        crc ^= static_cast<uint32_t>(word & 0xFFFFFFFFu);
        const uint32_t hi = static_cast<uint32_t>(word >> 32);
        crc = t.slice[7][(crc) & 0xFF] ^
              t.slice[6][(crc >> 8) & 0xFF] ^
              t.slice[5][(crc >> 16) & 0xFF] ^
              t.slice[4][(crc >> 24) & 0xFF] ^
              t.slice[3][(hi) & 0xFF] ^
              t.slice[2][(hi >> 8) & 0xFF] ^
              t.slice[1][(hi >> 16) & 0xFF] ^
              t.slice[0][(hi >> 24) & 0xFF];
        data += 8;
        size -= 8;
    }
    while (size > 0) {
        crc = t.slice[0][(crc ^ *data) & 0xFF] ^ (crc >> 8);
        ++data;
        --size;
    }
    return ~crc;
}

}  // namespace mediaqc::fs
