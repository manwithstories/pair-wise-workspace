#include "report.hpp"

#include <algorithm>
#include <cstdio>
#include <fstream>
#include <stdexcept>

#include "ppm.hpp"

namespace {

std::string sanitize_note(std::string s) {
    for (char& c : s) {
        if (c == ',' || c == '\n' || c == '\r')
            c = ';';
    }
    return s;
}

} // namespace

void write_frames_csv(const std::string& path,
                      const std::vector<FrameRow>& rows) {
    std::ofstream out(path, std::ios::trunc);
    if (!out)
        throw std::runtime_error("cannot write: " + path);
    out << "index,timestamp_us,status,intensity,bits,note\n";
    char buf[64];
    for (const FrameRow& r : rows) {
        out << r.index << ',' << r.timestamp_us << ','
            << (r.ok ? "ok" : "skipped") << ',';
        if (r.has_intensity) {
            std::snprintf(buf, sizeof buf, "%.6f", r.intensity);
            out << buf;
        }
        out << ',';
        if (r.has_bits)
            out << r.bits;
        out << ',' << sanitize_note(r.note) << '\n';
    }
    out.flush();
    if (!out)
        throw std::runtime_error("write failed: " + path);
}

void write_segments_csv(const std::string& path,
                        const std::vector<Segment>& segments) {
    std::ofstream out(path, std::ios::trunc);
    if (!out)
        throw std::runtime_error("cannot write: " + path);
    out << "begin,end\n";
    for (const Segment& s : segments)
        out << s.begin << ',' << s.end << '\n';
    out.flush();
    if (!out)
        throw std::runtime_error("write failed: " + path);
}

FrameBuffer render_histogram(const std::array<uint64_t, 10>& hist) {
    constexpr std::size_t kW = 256, kH = 128;
    FrameBuffer img(kW, kH, 1);
    const uint64_t maxc = *std::max_element(hist.begin(), hist.end());
    for (std::size_t x = 0; x < kW; ++x) {
        const std::size_t bucket = x * 10 / kW;
        const uint64_t bar = maxc ? (hist[bucket] * (kH - 1)) / maxc : 0;
        for (std::size_t y = 0; y < kH; ++y)
            img.row(y)[x] = (kH - 1 - y < bar) ? 255 : 0;
    }
    return img;
}

uint64_t pixel_sum(const FrameBuffer& img) {
    uint64_t sum = 0;
    const std::size_t row_bytes = img.width() * img.bpp();
    for (std::size_t y = 0; y < img.height(); ++y) {
        const uint8_t* r = img.row(y);
        for (std::size_t i = 0; i < row_bytes; ++i)
            sum += r[i];
    }
    return sum;
}

uint64_t fnv1a64(const FrameBuffer& img) {
    uint64_t h = UINT64_C(14695981039346656037);
    const std::size_t row_bytes = img.width() * img.bpp();
    for (std::size_t y = 0; y < img.height(); ++y) {
        const uint8_t* r = img.row(y);
        for (std::size_t i = 0; i < row_bytes; ++i) {
            h ^= r[i];
            h *= UINT64_C(1099511628211);
        }
    }
    return h;
}

HistogramSummary write_histogram_ppm(const std::string& path,
                                     const std::array<uint64_t, 10>& hist) {
    FrameBuffer img = render_histogram(hist);
    HistogramSummary s;
    s.pixels_sum = pixel_sum(img);
    s.fnv = fnv1a64(img);
    write_ppm_gray(path, img);
    return s;
}
