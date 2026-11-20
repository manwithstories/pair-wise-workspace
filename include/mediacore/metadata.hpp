#pragma once

#include <string>
#include <vector>

#include "mediacore/error.hpp"

namespace mediacore {

// Normalized color description. Values use the libavutil enum integer so
// comparisons stay exact; names are resolved for the report.
struct ColorMetadata {
    int range = 0;        // AVColorRange, 0 == unspecified
    int primaries = 0;    // AVColorPrimaries
    int transfer = 0;     // AVColorTransferCharacteristic
    int matrix = 0;       // AVColorSpace

    bool rangeKnown() const { return range != 0; }
    bool primariesKnown() const { return primaries != 0; }
    bool transferKnown() const { return transfer != 0; }
    bool matrixKnown() const { return matrix != 0; }
};

const char* colorRangeName(int value);
const char* colorPrimariesName(int value);
const char* colorTransferName(int value);
const char* colorMatrixName(int value);

enum class ColorPolicy {
    Strict,     // any conflict is a fatal error
    PreferTag,  // keep the stream-tagged value, report the disagreement
    PreferSrgb, // resolve conflicts toward BT.709/sRGB defaults
};

const char* toString(ColorPolicy policy);
ColorPolicy parseColorPolicy(const std::string& text);

struct ColorConflict {
    std::string field;
    int expected = 0;
    int actual = 0;
    std::string decision;  // documented choice rationale
};

struct ColorReport {
    ColorMetadata resolved;
    bool incomplete = false;  // one or more fields unknown
    std::vector<ColorConflict> conflicts;
};

// Check one stream's color metadata and normalize it under the requested
// policy. Missing fields are marked unknown (never invented); conflicts
// across segments produce documented decisions or fatal errors.
ColorReport checkColorMetadata(const std::string& track,
                               const std::vector<ColorMetadata>& segments,
                               ColorPolicy policy, ErrorLog& errors);

// Human-readable multi-line rendering used in terminal output.
std::string toString(const ColorMetadata& c);

}  // namespace mediacore
