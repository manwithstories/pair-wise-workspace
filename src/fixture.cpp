#include "fixture.hpp"

#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <stdexcept>

#include "frame.hpp"
#include "ppm.hpp"

void generate_fixture(const std::string& out_dir) {
    namespace fs = std::filesystem;
    std::error_code ec;
    fs::create_directories(out_dir, ec);
    if (ec)
        throw std::runtime_error("cannot create directory: " + out_dir);
    const std::string manifest_path =
        (fs::path(out_dir) / "frames.tsv").string();
    if (fs::exists(manifest_path))
        throw std::runtime_error("output not empty: " + manifest_path);

    // Frame i is white iff the black/white state after the flips at
    // indices 3,4,5,9,10 leaves it white.
    const bool white[12] = {false, false, false, true, false, true,
                            true,  true,  true,  false, true, true};
    constexpr uint64_t kBaseTs = UINT64_C(1700000000000000);
    constexpr uint64_t kStepUs = 100000;

    std::ofstream tsv(manifest_path, std::ios::trunc);
    if (!tsv)
        throw std::runtime_error("cannot write: " + manifest_path);
    for (int i = 0; i < 12; ++i) {
        FrameBuffer f(16, 16, 1);
        const uint8_t v = white[i] ? 255 : 0;
        for (std::size_t y = 0; y < f.height(); ++y)
            std::memset(f.row(y), v, f.width());
        char name[32];
        std::snprintf(name, sizeof name, "frame_%03d.ppm", i);
        write_ppm_gray((fs::path(out_dir) / name).string(), f);
        tsv << (kBaseTs + static_cast<uint64_t>(i) * kStepUs) << '\t' << name
            << '\n';
    }
    tsv.flush();
    if (!tsv)
        throw std::runtime_error("write failed: " + manifest_path);
}
