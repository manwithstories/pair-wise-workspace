#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"
#include "subtitle_audit/digest.hpp"

namespace audit {

struct ProbeEvent {
    int index = 0;
    int line = 0;
    bool visible = false;
    PixelBox bbox;
    std::string alpha_fnv1a;
    std::vector<std::string> missing_fonts;
};

struct ProbeResult {
    bool ok = true;
    std::string error;
    int64_t frame = 0;
    int width = 1920;
    int height = 1080;
    int visible = 0;
    std::vector<ProbeEvent> events;
    std::vector<std::string> missing_fonts;
    int max_width = 0;
    int max_height = 0;
};

ProbeResult render_probe(const AssDocument &doc, int64_t frame,
                         const std::string &dump_alpha_dir);

}  // namespace audit
