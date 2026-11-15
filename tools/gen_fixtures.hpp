#pragma once

#include <cstdint>
#include <string>

namespace audit {

struct FixtureOptions {
    std::string output_dir;
    int count = 12;
    std::uint64_t seed = 20261116;
};

bool generate_fixtures(const FixtureOptions &options, std::string &error);

}  // namespace audit
