#pragma once

#include <memory>
#include <vector>

#include "subqc/model.hpp"

namespace subqc {

class RenderProbe {
public:
    static std::unique_ptr<RenderProbe> create(const RenderOptions &options,
                                               std::vector<Diagnostic> &diagnostics);
    ~RenderProbe();
    RenderProbe(RenderProbe &&) noexcept;
    RenderProbe &operator=(RenderProbe &&) noexcept;
    RenderProbe(const RenderProbe &) = delete;
    RenderProbe &operator=(const RenderProbe &) = delete;

    bool render_file(AnalyzedFile &file, const RenderOptions &options);

private:
    struct Impl;
    explicit RenderProbe(std::unique_ptr<Impl> impl);
    std::unique_ptr<Impl> impl_;
};

std::uint32_t crc32_bytes(const std::uint8_t *data, std::size_t size);

}  // namespace subqc
