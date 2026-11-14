#pragma once

#include <cstdint>
#include <filesystem>
#include <string>

namespace subqc {

struct FixtureSummary {
    int files = 0;
    int events = 0;
    int styles = 0;
};

bool generate_fixtures(const std::filesystem::path &out, int event_count, std::uint64_t seed,
                       FixtureSummary &summary, std::string &error);

}  // namespace subqc
