#include "ppm.hpp"

#include <cctype>
#include <cstring>
#include <fstream>
#include <stdexcept>
#include <vector>

namespace {

// Tokenizer for the PPM header: whitespace-separated tokens with '#'
// comments running to end of line.
struct TokenReader {
    std::istream& in;

    void skip_ws_and_comments() {
        for (;;) {
            int c = in.peek();
            if (c == EOF)
                return;
            if (c == '#') {
                in.ignore(1024, '\n');
                continue;
            }
            if (std::isspace(c)) {
                in.get();
                continue;
            }
            return;
        }
    }

    std::string token() {
        skip_ws_and_comments();
        std::string t;
        for (;;) {
            int c = in.peek();
            if (c == EOF || std::isspace(c) || c == '#')
                break;
            t.push_back(static_cast<char>(in.get()));
        }
        return t;
    }
};

uint64_t parse_uint(const std::string& t, const char* what,
                    const std::string& path) {
    if (t.empty())
        throw std::runtime_error("PPM " + path + ": missing " + what);
    uint64_t v = 0;
    for (char c : t) {
        if (c < '0' || c > '9')
            throw std::runtime_error("PPM " + path + ": bad " + what);
        if (v > (UINT64_MAX - 9) / 10)
            throw std::runtime_error("PPM " + path + ": " + what + " overflow");
        v = v * 10 + static_cast<uint64_t>(c - '0');
    }
    return v;
}

} // namespace

FrameBuffer read_ppm(const std::string& path) {
    std::ifstream in(path, std::ios::binary);
    if (!in)
        throw std::runtime_error("cannot open: " + path);

    TokenReader tr{in};
    if (tr.token() != "P6")
        throw std::runtime_error("PPM " + path + ": not a binary P6 file");
    const uint64_t w = parse_uint(tr.token(), "width", path);
    const uint64_t h = parse_uint(tr.token(), "height", path);
    const uint64_t maxval = parse_uint(tr.token(), "maxval", path);
    if (maxval != 255)
        throw std::runtime_error("PPM " + path + ": maxval must be 255");
    if (w == 0 || h == 0)
        throw std::runtime_error("PPM " + path + ": zero dimension");

    // Dimension multiplication overflow / sanity guard, checked before
    // any allocation is attempted.
    const uint64_t kMaxPixels = uint64_t{1} << 31;
    if (w > kMaxPixels / h)
        throw std::runtime_error("PPM " + path + ": dimension overflow");
    const uint64_t pixels = w * h;
    if (pixels > kMaxPixels)
        throw std::runtime_error("PPM " + path + ": dimensions too large");

    // Exactly one whitespace byte separates the header from the raster.
    const int sep = in.get();
    if (sep == EOF || !std::isspace(sep))
        throw std::runtime_error("PPM " + path + ": malformed header");

    FrameBuffer fb(static_cast<std::size_t>(w), static_cast<std::size_t>(h), 3);
    std::vector<uint8_t> rowbuf(static_cast<std::size_t>(w) * 3);
    for (uint64_t y = 0; y < h; ++y) {
        in.read(reinterpret_cast<char*>(rowbuf.data()),
                static_cast<std::streamsize>(rowbuf.size()));
        if (!in)
            throw std::runtime_error("PPM " + path + ": truncated raster");
        std::memcpy(fb.row(static_cast<std::size_t>(y)), rowbuf.data(),
                    rowbuf.size());
    }
    if (in.peek() != EOF)
        throw std::runtime_error("PPM " + path + ": trailing bytes after raster");
    return fb;
}

void write_ppm_gray(const std::string& path, const FrameBuffer& img) {
    if (img.bpp() != 1)
        throw std::invalid_argument("write_ppm_gray expects a 1-byte frame");
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out)
        throw std::runtime_error("cannot write: " + path);
    out << "P6\n" << img.width() << " " << img.height() << "\n255\n";
    std::vector<uint8_t> row(img.width() * 3);
    for (std::size_t y = 0; y < img.height(); ++y) {
        const uint8_t* src = img.row(y);
        for (std::size_t x = 0; x < img.width(); ++x) {
            row[3 * x] = row[3 * x + 1] = row[3 * x + 2] = src[x];
        }
        out.write(reinterpret_cast<const char*>(row.data()),
                  static_cast<std::streamsize>(row.size()));
    }
    out.flush();
    if (!out)
        throw std::runtime_error("write failed: " + path);
}
