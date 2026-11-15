#include "subtitle_audit/digest.hpp"

#include <filesystem>
#include <fstream>
#include <iomanip>
#include <sstream>

namespace audit {
namespace {

uint64_t fnv1a_update(uint64_t hash, const unsigned char *data, std::size_t size) {
    for (std::size_t i = 0; i < size; ++i) {
        hash ^= data[i];
        hash *= UINT64_C(1099511628211);
    }
    return hash;
}

void hash_int(uint64_t &hash, int value) {
    const unsigned char bytes[4] = {
        static_cast<unsigned char>(value & 0xff),
        static_cast<unsigned char>((value >> 8) & 0xff),
        static_cast<unsigned char>((value >> 16) & 0xff),
        static_cast<unsigned char>((value >> 24) & 0xff)};
    hash = fnv1a_update(hash, bytes, sizeof(bytes));
}

int plane_alpha(const AlphaPlane &plane, int x, int y) {
    if (x < plane.x || y < plane.y || x >= plane.x + plane.w || y >= plane.y + plane.h)
        return 0;
    return plane.pixels[static_cast<std::size_t>(y - plane.y) * plane.stride +
                        static_cast<std::size_t>(x - plane.x)];
}

int composite_alpha(const std::vector<AlphaPlane> &planes, int x, int y) {
    int out = 0;
    for (const AlphaPlane &plane : planes) {
        const int src = plane_alpha(plane, x, y);
        out = src + out * (255 - src) / 255;
    }
    return out;
}

}  // namespace

std::string alpha_digest(const std::vector<AlphaPlane> &planes) {
    uint64_t hash = UINT64_C(1469598103934665603);
    for (const AlphaPlane &plane : planes) {
        hash_int(hash, plane.x);
        hash_int(hash, plane.y);
        hash_int(hash, plane.w);
        hash_int(hash, plane.h);
        for (int row = 0; row < plane.h; ++row) {
            const auto *line = plane.pixels + static_cast<std::size_t>(row) * plane.stride;
            hash = fnv1a_update(hash, line, static_cast<std::size_t>(plane.w));
        }
    }
    std::ostringstream text;
    text << std::hex << std::setw(16) << std::setfill('0') << hash;
    return text.str();
}

bool write_alpha_ppm(const std::string &path, const std::vector<AlphaPlane> &planes,
                     const PixelBox &box) {
    std::error_code error;
    const std::filesystem::path output(path);
    if (output.has_parent_path())
        std::filesystem::create_directories(output.parent_path(), error);
    if (error) return false;
    std::ofstream file(path, std::ios::binary);
    if (!file) return false;
    file << "P6\n" << box.w << ' ' << box.h << "\n255\n";
    for (int row = 0; row < box.h; ++row) {
        for (int col = 0; col < box.w; ++col) {
            const unsigned char value = static_cast<unsigned char>(
                composite_alpha(planes, box.x + col, box.y + row));
            const char pixel[3] = {static_cast<char>(value), static_cast<char>(value),
                                   static_cast<char>(value)};
            file.write(pixel, 3);
        }
    }
    return static_cast<bool>(file);
}

}  // namespace audit
