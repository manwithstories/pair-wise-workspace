#pragma once

#include <array>
#include <cstdint>
#include <string>
#include <vector>

namespace mp4 {

struct BoxNode {
    std::string type;
    std::array<uint8_t, 16> uuid{};
    uint64_t offset = 0;
    uint64_t size = 0;
    uint64_t header_size = 0;
    uint64_t payload_offset = 0;
    uint64_t payload_size = 0;
    bool has_uuid = false;
    std::vector<BoxNode> children;

    const BoxNode* find(const std::string& child_type) const;
    std::vector<const BoxNode*> find_all(const std::string& child_type) const;
};

class BoxTree {
public:
    explicit BoxTree(const std::vector<uint8_t>& data);

    const std::vector<BoxNode>& roots() const noexcept { return roots_; }
    const BoxNode* find_root(const std::string& type) const;

private:
    std::vector<BoxNode> roots_;
};

const BoxNode* find_path(const BoxNode& root, const std::vector<std::string>& path);

}  // namespace mp4
