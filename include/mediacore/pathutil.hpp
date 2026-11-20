#pragma once

#include <filesystem>
#include <optional>
#include <string>

namespace mediacore {

// Filesystem path containment. Every manifest-referenced segment and every
// output path must resolve inside one of the configured roots; symlinks are
// lexically normalized (no network access, no canonicalization of missing
// files). Returns the contained path or nullopt when it escapes.
std::optional<std::filesystem::path> constrainPath(
    const std::filesystem::path& root,
    const std::filesystem::path& candidate);

// Resolve a possibly relative URI against a manifest directory, then
// constrain it. Rejects URL schemes other than plain relative paths.
std::optional<std::filesystem::path> resolveManifestUri(
    const std::filesystem::path& manifestDir,
    const std::string& uri,
    const std::filesystem::path& root);

}  // namespace mediacore
