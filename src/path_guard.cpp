#include "mediacore/path_guard.hpp"

#include <system_error>

namespace fs = std::filesystem;

namespace mediacore {

namespace {

// 词法规范化（不触碰文件系统），统一 lexically_normal 后比较前缀
bool isWithin(const fs::path& base, const fs::path& candidate) {
    auto b = base.lexically_normal();
    auto c = candidate.lexically_normal();
    auto bs = b.string();
    auto cs = c.string();
    if (bs == cs) {
        return true;
    }
    if (bs.back() != fs::path::preferred_separator) {
        bs.push_back(fs::path::preferred_separator);
    }
    return cs.rfind(bs, 0) == 0;
}

} // namespace

Result<fs::path> safeJoin(const fs::path& baseDir,
                          const fs::path& userPath) noexcept {
    std::error_code ec;
    fs::path base = fs::weakly_canonical(baseDir, ec);
    if (ec) {
        base = fs::absolute(baseDir);
    }

    // 绝对路径直接逃逸的一律拒绝
    if (userPath.is_absolute()) {
        return MediaError{MediaErrorCode::PathTraversal,
                          "absolute path outside workspace rejected: " +
                              userPath.string(),
                          false};
    }

    fs::path joined = fs::weakly_canonical(base / userPath, ec);
    if (ec) {
        joined = (base / userPath).lexically_normal();
    }
    if (!isWithin(base, joined)) {
        return MediaError{MediaErrorCode::PathTraversal,
                          "path traversal rejected: " + userPath.string(),
                          false};
    }

    // 符号链接解析后若指向工作区之外同样拒绝
    std::error_code linkEc;
    fs::path resolved = fs::weakly_canonical(joined, linkEc);
    if (!linkEc && !isWithin(base, resolved)) {
        return MediaError{MediaErrorCode::PathTraversal,
                          "symlink escapes workspace: " + userPath.string(),
                          false};
    }
    return joined;
}

Result<fs::path> ensureDirectory(const fs::path& dir) noexcept {
    std::error_code ec;
    fs::create_directories(dir, ec);
    if (ec) {
        return MediaError{MediaErrorCode::WriteFailed,
                          "cannot create directory " + dir.string() + ": " +
                              ec.message(),
                          false};
    }
    return dir;
}

} // namespace mediacore
