#include "mediacore/pathutil.hpp"

#include <system_error>

namespace mediacore {

namespace fs = std::filesystem;

std::optional<fs::path> constrainPath(const fs::path& root,
                                      const fs::path& candidate) {
    std::error_code ec;
    fs::path normRoot = fs::weakly_canonical(root, ec);
    if (ec) normRoot = root.lexically_normal();
    fs::path normCand = fs::weakly_canonical(candidate, ec);
    if (ec) normCand = candidate.lexically_normal();

    auto rootIt = normRoot.begin();
    auto candIt = normCand.begin();
    while (rootIt != normRoot.end()) {
        if (candIt == normCand.end() || *candIt != *rootIt)
            return std::nullopt;
        ++rootIt;
        ++candIt;
    }
    return normCand;
}

std::optional<fs::path> resolveManifestUri(const fs::path& manifestDir,
                                           const std::string& uri,
                                           const fs::path& root) {
    if (uri.empty() || uri.starts_with("http://") ||
        uri.starts_with("https://") || uri.starts_with("file://"))
        return std::nullopt;
#ifdef _WIN32
    if (uri.size() >= 2 && uri[1] == ':') return std::nullopt;
#endif
    fs::path joined = manifestDir / fs::path(uri);
    return constrainPath(root, joined);
}

}  // namespace mediacore
