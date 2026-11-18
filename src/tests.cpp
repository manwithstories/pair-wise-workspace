// Unit / regression tests for the motion analyzer modules.
#include <array>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>
#include <unistd.h>

#include "allocator.hpp"
#include "diff.hpp"
#include "fixture.hpp"
#include "ppm.hpp"
#include "report.hpp"
#include "segments.hpp"
#include "sequence.hpp"

static int g_checks = 0, g_failures = 0;

#define CHECK(cond)                                                        \
    do {                                                                   \
        ++g_checks;                                                        \
        if (!(cond)) {                                                     \
            ++g_failures;                                                  \
            std::cerr << "FAIL " << __FILE__ << ":" << __LINE__ << " " #cond \
                      << "\n";                                             \
        }                                                                  \
    } while (0)

#define CHECK_THROW(stmt)                                  \
    do {                                                   \
        ++g_checks;                                        \
        bool threw = false;                                \
        try {                                              \
            stmt;                                          \
        } catch (...) {                                    \
            threw = true;                                  \
        }                                                  \
        if (!threw) {                                      \
            ++g_failures;                                  \
            std::cerr << "FAIL " << __FILE__ << ":" << __LINE__ \
                      << " expected throw: " #stmt "\n";   \
        }                                                  \
    } while (0)

static std::string tmp_dir() {
    const std::string p =
        (std::filesystem::temp_directory_path() /
         ("motion_tests_" + std::to_string(::getpid())))
            .string();
    std::filesystem::create_directories(p);
    return p;
}

static void write_file(const std::string& path, const std::string& data) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    out << data;
}

// Strided frame buffer: padding must never enter the diff statistics.
static void test_stride_frame() {
    FrameBuffer a(5, 3, 3, 64), b(5, 3, 3, 64);
    CHECK(a.stride() == 64);
    for (std::size_t y = 0; y < 3; ++y)
        for (std::size_t x = 0; x < 5; ++x) {
            for (int c = 0; c < 3; ++c) {
                a.row(y)[3 * x + c] = static_cast<uint8_t>(10 + 10 * c);
                b.row(y)[3 * x + c] = static_cast<uint8_t>(10 + 10 * c);
            }
        }
    b.row(1)[3 * 2 + 0] = 200;
    b.row(1)[3 * 2 + 1] = 100;
    b.row(1)[3 * 2 + 2] = 50;
    // Poison the padding differently in the two frames.
    for (std::size_t y = 0; y < 3; ++y)
        for (std::size_t k = 15; k < 64; ++k) {
            a.row(y)[k] = 0xFF;
            b.row(y)[k] = 0x00;
        }
    const int la = diff::luma(10, 20, 30), lb = diff::luma(200, 100, 50);
    CHECK(la == 18 && lb == 124);
    CHECK(diff::abs_luma_sum(a, b) == 106);
    CHECK(std::fabs(diff::intensity(a, b) - 106.0 / (255.0 * 15)) < 1e-12);
    CHECK_THROW(FrameBuffer(SIZE_MAX / 2, 4, 3, 64)); // row size overflow
    CHECK_THROW(FrameBuffer(4, SIZE_MAX / 2, 3, 64)); // buffer size overflow
}

static bool segs_eq(const std::vector<Segment>& got,
                    std::initializer_list<Segment> want) {
    if (got.size() != want.size())
        return false;
    std::size_t i = 0;
    for (const Segment& s : want) {
        if (got[i].begin != s.begin || got[i].end != s.end)
            return false;
        ++i;
    }
    return true;
}

static void test_segments() {
    { // equal thresholds: enter == exit == 0.5
        Segmenter s({0.5, 0.5, 1, 0});
        s.push(0, 0.5); s.push(1, 0.5); s.push(2, 0.1); s.push(3, 0.6);
        s.finish();
        CHECK(segs_eq(s.segments(), {{0, 2}, {3, 4}}));
    }
    { // tail segment: still open at EOF, settled by finish()
        Segmenter s({0.8, 0.4, 2, 0});
        s.push(0, 0.9); s.push(1, 0.9);
        s.finish();
        CHECK(segs_eq(s.segments(), {{0, 2}}));
    }
    { // gap fallback: gap of 1 tolerated once, exceeded -> close at last high
        Segmenter s({0.8, 0.4, 1, 1});
        s.push(0, 0.9); s.push(1, 0.1); s.push(2, 0.9); // gap 1 tolerated
        s.push(3, 0.1); s.push(4, 0.1);                 // gap 2 -> close
        s.push(5, 0.9);
        s.finish();
        CHECK(segs_eq(s.segments(), {{0, 3}, {5, 6}}));
    }
    { // min_frames not reached -> no segment; cut() breaks the run
        Segmenter s({0.8, 0.4, 2, 0});
        s.push(0, 0.9);
        s.cut();
        s.push(1, 0.9); s.push(2, 0.9);
        s.finish();
        CHECK(segs_eq(s.segments(), {{1, 3}}));
    }
}

static void test_allocator() {
    { // rounding: largest remainder wins
        auto got = allocate_bits({0.5, 0.5, 0.0}, 10);
        CHECK((got == std::vector<uint64_t>{4, 4, 2}));
    }
    { // tie on remainder -> lower index first
        auto got = allocate_bits({0.0, 0.0, 0.0}, 4);
        CHECK((got == std::vector<uint64_t>{2, 1, 1}));
    }
    { // conservation on uneven weights
        auto got = allocate_bits({0.1, 0.9, 0.3, 0.7}, 1000);
        uint64_t sum = 0;
        for (uint64_t b : got)
            sum += b;
        CHECK(sum == 1000);
    }
    { // fixture shape: 1+3s weights, exact split
        std::vector<double> v(6, 0.0);
        v.insert(v.end(), 5, 1.0);
        auto got = allocate_bits(v, 26000);
        bool ok = true;
        for (std::size_t i = 0; i < got.size(); ++i)
            ok &= got[i] == (i < 6 ? 1000 : 4000);
        CHECK(ok);
    }
    CHECK_THROW(allocate_bits({}, 5));
}

static void test_ppm(const std::string& dir) {
    const std::string ok = dir + "/ok.ppm";
    write_file(ok, "P6\n2 2\n255\n" + std::string(12, '\x7f'));
    FrameBuffer f = read_ppm(ok);
    CHECK(f.width() == 2 && f.height() == 2 && f.row(1)[3] == 0x7f);
    write_file(dir + "/trunc.ppm", "P6\n2 2\n255\n" + std::string(5, '\x01'));
    CHECK_THROW(read_ppm(dir + "/trunc.ppm"));
    write_file(dir + "/maxval.ppm",
               "P6\n2 2\n254\n" + std::string(12, '\x00'));
    CHECK_THROW(read_ppm(dir + "/maxval.ppm"));
    write_file(dir + "/huge.ppm", "P6\n4294967296 4294967296\n255\n");
    CHECK_THROW(read_ppm(dir + "/huge.ppm"));
    write_file(dir + "/trail.ppm", "P6\n1 1\n255\n" + std::string(4, '\x00'));
    CHECK_THROW(read_ppm(dir + "/trail.ppm"));
    write_file(dir + "/magic.ppm", "P3\n1 1\n255\n0 0 0\n");
    CHECK_THROW(read_ppm(dir + "/magic.ppm"));
    write_file(dir + "/comment.ppm",
               "P6\n# c\n2 1\n255\n" + std::string(6, '\x05'));
    CHECK(read_ppm(dir + "/comment.ppm").row(0)[5] == 0x05);
}

static void test_manifest(const std::string& dir) {
    write_file(dir + "/good.tsv", "100\ta.ppm\n200\tsub/b.ppm\n");
    Manifest m = load_manifest(dir + "/good.tsv");
    CHECK(m.entries.size() == 2 && m.entries[1].timestamp_us == 200);
    CHECK(manifest_entry_path(m, m.entries[0]) == dir + "/a.ppm");
    write_file(dir + "/esc.tsv", "100\t../evil.ppm\n");
    CHECK_THROW(load_manifest(dir + "/esc.tsv"));
    write_file(dir + "/abs.tsv", "100\t/abs.ppm\n");
    CHECK_THROW(load_manifest(dir + "/abs.tsv"));
    write_file(dir + "/dup.tsv", "100\ta.ppm\n100\tb.ppm\n");
    CHECK_THROW(load_manifest(dir + "/dup.tsv"));
    write_file(dir + "/desc.tsv", "200\ta.ppm\n100\tb.ppm\n");
    CHECK_THROW(load_manifest(dir + "/desc.tsv"));
    write_file(dir + "/notab.tsv", "100 a.ppm\n");
    CHECK_THROW(load_manifest(dir + "/notab.tsv"));
}

// Corrupt recovery: a cut chain re-establishes the reference on the next
// valid frame, which then yields no intensity itself.
static void test_framestream_recovery() {
    FrameBuffer f0(4, 4, 3), f1(4, 4, 3), f2(4, 4, 3), f3(4, 4, 3);
    for (std::size_t y = 0; y < 4; ++y)
        for (std::size_t x = 0; x < 12; ++x) {
            f0.row(y)[x] = 0;
            f1.row(y)[x] = 255;
            f2.row(y)[x] = 7;
            f3.row(y)[x] = 9;
        }
    FrameStream fs;
    double s = -1;
    std::string err;
    CHECK(fs.push(f0, s, err) == PushResult::Reference);
    CHECK(fs.push(f1, s, err) == PushResult::Intensity && s == 1.0);
    fs.cut(); // skipped frame breaks the chain
    CHECK(fs.push(f2, s, err) == PushResult::Reference);
    CHECK(fs.push(f3, s, err) == PushResult::Intensity);
    CHECK(s == diff::intensity(f2, f3));
    FrameBuffer other(8, 8, 3);
    CHECK(fs.push(other, s, err) == PushResult::SizeMismatch);
    fs.cut();
    CHECK(fs.push(f0, s, err) == PushResult::Reference);
}

static void test_histogram() {
    std::array<uint64_t, 10> h{6, 0, 0, 0, 0, 0, 0, 0, 0, 5};
    FrameBuffer img = render_histogram(h);
    CHECK(img.width() == 256 && img.height() == 128);
    CHECK(img.row(127)[0] == 255);      // bucket 0 bar reaches the bottom
    CHECK(img.row(127)[255] == 255);    // bucket 9 bar
    CHECK(img.row(127)[128] == 0);      // bucket 5 is empty
    CHECK(img.row(0)[0] == 0);          // bar height capped at 127
    CHECK(render_histogram(h).row(60)[255] == img.row(60)[255]); // stable
    CHECK(pixel_sum(img) == pixel_sum(render_histogram(h)));
    CHECK(fnv1a64(img) == fnv1a64(render_histogram(h)));
}

// Golden fixture pipeline: the acceptance numbers end to end.
static void test_fixture_golden(const std::string& dir) {
    const std::string fix = dir + "/fixture";
    generate_fixture(fix);
    Manifest m = load_manifest(fix + "/frames.tsv");
    CHECK(m.entries.size() == 12);
    CHECK(m.entries[1].timestamp_us - m.entries[0].timestamp_us == 100000);
    FrameStream stream;
    Segmenter seg({0.8, 0.4, 2, 0});
    std::vector<double> intensities;
    std::array<uint64_t, 10> hist{};
    for (const ManifestEntry& e : m.entries) {
        FrameBuffer cur = read_ppm(manifest_entry_path(m, e));
        double s = 0;
        std::string err;
        if (stream.push(std::move(cur), s, err) == PushResult::Intensity) {
            intensities.push_back(s);
            ++hist[s == 1.0 ? 9 : 0];
            seg.push(e.index, s);
        }
    }
    seg.finish();
    CHECK(intensities.size() == 11);
    CHECK(hist[0] == 6 && hist[9] == 5);
    CHECK(segs_eq(seg.segments(), {{3, 6}, {9, 11}}));
    auto bits = allocate_bits(intensities, 26000);
    bool ok = true;
    uint64_t sum = 0;
    for (std::size_t i = 0; i < bits.size(); ++i) {
        ok &= bits[i] == (intensities[i] == 1.0 ? 4000 : 1000);
        sum += bits[i];
    }
    CHECK(ok && sum == 26000);
}

int main() {
    const std::string dir = tmp_dir();
    test_stride_frame();
    test_segments();
    test_allocator();
    test_ppm(dir);
    test_manifest(dir);
    test_framestream_recovery();
    test_histogram();
    test_fixture_golden(dir);
    std::filesystem::remove_all(dir);
    std::cout << "checks=" << g_checks << " failures=" << g_failures << "\n";
    return g_failures ? 1 : 0;
}
