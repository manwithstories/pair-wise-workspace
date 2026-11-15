#include "framebuffer.hpp"
#include "hash.hpp"
#include "time_map.hpp"
#include "timeline.hpp"
#include "transition_kernel.hpp"

#include <chrono>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <stdexcept>
#include <string>

namespace {

using namespace bake;

int checks = 0;

void expect(bool condition, const std::string& message) {
    if (!condition) throw std::runtime_error(message);
    ++checks;
}

template <typename Function>
void expectInputError(Function function, const std::string& message) {
    try {
        function();
    } catch (const BakeError& error) {
        if (error.code != ExitCode::invalid_input) {
            throw std::runtime_error(message + ": wrong exit code");
        }
        ++checks;
        return;
    }
    throw std::runtime_error(message + ": expected input error");
}

template <typename Function>
void expectInvariantError(Function function, const std::string& message) {
    try {
        function();
    } catch (const BakeError& error) {
        if (error.code != ExitCode::invariant) {
            throw std::runtime_error(message + ": wrong exit code");
        }
        ++checks;
        return;
    }
    throw std::runtime_error(message + ": expected invariant error");
}

FrameBuffer makePattern(uint8_t offset) {
    FrameBuffer frame(9, 7);
    for (int y = 0; y < frame.height; ++y) {
        for (int x = 0; x < frame.width; ++x) {
            frame.setPixel(x, y, static_cast<uint8_t>(x * 9 + y + offset),
                           static_cast<uint8_t>(x + y * 13 + offset),
                           static_cast<uint8_t>(x * 3 + y * 7 + offset));
        }
    }
    return frame;
}

void testKernels() {
    expect(kernelRegistry().size() == 5U, "registry size");
    const FrameBuffer front = makePattern(0);
    const FrameBuffer back = makePattern(97);
    const uint64_t front_hash = fnv1a64(front);
    const uint64_t back_hash = fnv1a64(back);
    FrameBuffer destination(9, 7);
    for (const KernelEntry& entry : kernelRegistry()) {
        applyKernel(entry.name, front, back, destination, 0, KernelParams{7, 0});
        expect(fnv1a64(destination) == front_hash, entry.name + " front endpoint");
        applyKernel(entry.name, front, back, destination, 65536, KernelParams{7, 1});
        expect(fnv1a64(destination) == back_hash, entry.name + " back endpoint");
    }
    expectInputError([&]() {
        applyKernel("linear_blend", front, back, destination, 65537,
                    KernelParams{7, 2});
    }, "progress bounds");
}

void testTimeMap() {
    const TimeMap hold({{4, 5, 0, 1}}, 10, false);
    expect(hold.map(0).source_frame == 5 && hold.map(3).source_frame == 5,
           "hold source");
    expect(hold.map(0).progress_q16 == 0 && hold.map(3).progress_q16 == 0,
           "hold progress");
    const TimeMap reverse({{5, 9, -1, 1}}, 10, false);
    for (int64_t i = 1; i < reverse.totalFrames(); ++i) {
        expect(reverse.map(i).source_frame < reverse.map(i - 1).source_frame,
               "reverse strictly decreases");
    }
    const TimeMap half({{6, 0, 1, 2}}, 10, false);
    expect(half.map(0).progress_q16 == 0, "half progress zero");
    expect(half.map(1).progress_q16 == 32768, "half progress half");
    expect(half.map(2).source_frame == 1, "half source integration");
    expectInputError([]() { (void)TimeMap({{2, 0, 1, 0}}, 4, false); },
                     "zero denominator");
    expectInputError([]() { (void)TimeMap({{4, 1, -1, 1}}, 4, false); },
                     "reverse crosses start");
    const TimeMap schedule({{6, 0, 1, 2}, {6, 3, 2, 1}, {6, 15, 3, 1},
                            {6, 33, 0, 1}, {6, 33, -1, 1}, {6, 27, 1, 1}},
                           35, true);
    expect(schedule.totalFrames() == 36, "speed schedule length");
}

void testPpmAndFrames() {
    const FrameBuffer frame = makePattern(3);
    const uint64_t hash = fnv1a64(frame);
    const auto stamp = std::chrono::steady_clock::now().time_since_epoch().count();
    const std::filesystem::path directory =
        std::filesystem::temp_directory_path() /
        ("pairwise_kernel_tests_" + std::to_string(stamp));
    std::error_code error;
    std::filesystem::create_directories(directory, error);
    try {
        const std::filesystem::path path = directory / "frame.ppm";
        writePpm(path.string(), frame, 0);
        const FrameBuffer restored = readPpm(path.string(), 0);
        expect(fnv1a64(restored) == hash, "P6 round trip");
        FrameBuffer untouched(3, 3);
        expect(!untouched.fullyInitialized(), "uninitialized frame detection");
        const std::filesystem::path bad = directory / "bad.ppm";
        {
            std::ofstream output(bad, std::ios::binary | std::ios::trunc);
            output << "P6\n1 1\n254\n";
            const char bytes[3] = {1, 2, 3};
            output.write(bytes, 3);
        }
        expectInputError([&]() { (void)readPpm(bad.string(), 0); },
                         "maxval rejection");
        uint8_t red = 0;
        uint8_t green = 0;
        uint8_t blue = 0;
        expectInvariantError(
            [&]() { frame.getPixel(9, 0, red, green, blue); },
            "bounds check");
    } catch (...) {
        std::filesystem::remove_all(directory, error);
        throw;
    }
    std::filesystem::remove_all(directory, error);
}

void testHash() {
    FrameBuffer a(1, 1);
    FrameBuffer b(1, 1);
    a.setPixel(0, 0, 0, 0, 0);
    b.setPixel(0, 0, 255, 10, 0);
    const FrameDiffStats diff = diffFrames(a, b);
    expect(diff.max_abs == 255, "max diff");
    expect(diff.changed_pixels == 1, "changed pixels");
    expect(diff.meanAbsDiff() == 88, "mean diff");
    expect(hashHex(fnv1a64(a)).size() == 16U, "hash width");
    expect(frameFileName(12) == "000012.ppm", "zero padded frame name");
}

}  // namespace

int main() {
    try {
        testKernels();
        testTimeMap();
        testPpmAndFrames();
        testHash();
        std::cout << "{\"target\":\"kernel_tests\",\"ok\":true,\"checks\":" << checks
                  << "}\n";
        return 0;
    } catch (const std::exception& error) {
        std::cout << "{\"target\":\"kernel_tests\",\"ok\":false,\"error\":\""
                  << error.what() << "\"}\n";
        return 5;
    }
}
