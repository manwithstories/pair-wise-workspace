#include "box_tree.h"

#include <unordered_set>

namespace fmp4 {

namespace {

constexpr uint32_t kMaxNestingDepth = 32;

std::vector<BoxNode> parseRange(const BoxReader& reader, uint64_t begin, uint64_t end,
                                uint32_t depth) {
    if (depth > kMaxNestingDepth) {
        throw parseError("box nesting exceeds 32 levels", begin);
    }
    std::vector<BoxNode> boxes;
    uint64_t position = begin;
    while (position < end) {
        const BoxHeader header = reader.readBoxHeader(position, end);
        BoxNode node;
        static_cast<Box&>(node) = Box{header.type, header.offset, header.size, header.header_size};
        if (isContainerType(header.type)) {
            node.children = parseRange(reader, node.payloadOffset(), node.offset + node.size,
                                       depth + 1);
        }
        boxes.push_back(std::move(node));
        position += header.size;
    }
    if (position != end) {
        throw parseError("misaligned box tree", position);
    }
    return boxes;
}

}  // namespace

BoxTree BoxTree::parse(const BoxReader& reader) {
    BoxTree tree;
    tree.roots_ = parseRange(reader, 0, reader.fileSize(), 0);
    return tree;
}

bool isContainerType(const std::string& type) {
    static const std::unordered_set<std::string> containers = {
        "moov", "trak", "mdia", "minf", "stbl", "mvex", "moof", "traf",
        "edts", "dinf", "udta", "ilst", "ipro", "sinf", "schi", "meco",
    };
    return containers.count(type) != 0;
}

const BoxNode* findChild(const BoxNode& parent, const std::string& type) {
    for (const BoxNode& child : parent.children) {
        if (child.type == type) {
            return &child;
        }
    }
    return nullptr;
}

std::vector<const BoxNode*> findChildren(const BoxNode& parent, const std::string& type) {
    std::vector<const BoxNode*> result;
    for (const BoxNode& child : parent.children) {
        if (child.type == type) {
            result.push_back(&child);
        }
    }
    return result;
}

}  // namespace fmp4

