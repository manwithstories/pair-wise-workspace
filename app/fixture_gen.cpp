#include <filesystem>
#include <iostream>
#include <string>

#include "mediacore/fixture.hpp"

// fixture_gen --out <dir>
// Generates the offline sample set (healthy, corrupt and boundary media)
// consumed by media_audit and the test suite.
int main(int argc, char** argv) {
    std::filesystem::path out = "fixtures";
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--out" && i + 1 < argc) {
            out = argv[++i];
        } else if (arg == "--help" || arg == "-h") {
            std::cout << "usage: fixture_gen --out <directory>\n";
            return 0;
        } else {
            std::cerr << "invalid argument: " << arg << "\n";
            std::cerr << "usage: fixture_gen --out <directory>\n";
            return 3;  // parameter or configuration error
        }
    }
    try {
        auto layout = mediacore::fixture::generate(out);
        std::cout << "generated " << layout.tracks.size()
                  << " tracks under " << out << "\n";
    } catch (const std::exception& e) {
        std::cerr << "fixture generation failed: " << e.what() << "\n";
        return 3;
    }
    return 0;
}
