#pragma once

#include "box_reader.h"

#include <cstdint>
#include <string>
#include <vector>

namespace fmp4 {

struct Box {
    std::string type;
    uint64_t offset = 0;
    uint64_t size = 0;
    uint64_t header_size = 0;

    uint64_t payloadOffset() const { return offset + header_size; }
    uint64_t payloadSize() const { return size - header_size; }
};

struct BoxNode : Box {
    std::vector<BoxNode> children;
};

class BoxTree {
public:
    static BoxTree parse(const BoxReader& reader);

    const std::vector<BoxNode>& roots() const { return roots_; }

private:
    std::vector<BoxNode> roots_;
};

bool isContainerType(const std::string& type);
const BoxNode* findChild(const BoxNode& parent, const std::string& type);
std::vector<const BoxNode*> findChildren(const BoxNode& parent, const std::string& type);

}  // namespace fmp4

