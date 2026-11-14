#include "subqc/style_chain.hpp"

#include <algorithm>
#include <cstdint>
#include <functional>
#include <map>
#include <set>
#include <sstream>

#include "subqc/ass_reader.hpp"

namespace subqc {
namespace {

struct Definition {
    RawStyle raw;
    StyleValues values;
    std::string parent_key;
    int state = 0;
    bool cycle = false;
    bool invalid = false;
};

std::string key_of(const std::string &name) { return lower_copy(trim_copy(name)); }

double number(const std::map<std::string, std::string> &fields, const std::string &name,
              double fallback = 0.0) {
    const auto it = fields.find(name);
    if (it == fields.end()) return fallback;
    try {
        std::size_t used = 0;
        const double value = std::stod(trim_copy(it->second), &used);
        return used == 0 ? fallback : value;
    } catch (...) {
        return fallback;
    }
}

int integer(const std::map<std::string, std::string> &fields, const std::string &name, int fallback = 0) {
    return static_cast<int>(number(fields, name, fallback));
}

std::string text(const std::map<std::string, std::string> &fields, const std::string &name) {
    const auto it = fields.find(name);
    return it == fields.end() ? std::string{} : trim_copy(it->second);
}

StyleValues parse_values(const RawStyle &raw) {
    StyleValues out;
    out.parent = text(raw.fields, "parentstyle");
    if (out.parent.empty()) out.parent = text(raw.fields, "parent");
    if (out.parent.empty()) out.parent = text(raw.fields, "inherits");
    out.font_name = text(raw.fields, "fontname");
    const std::string size = text(raw.fields, "fontsize");
    if (!size.empty()) out.font_size = number(raw.fields, "fontsize", 36.0);
    const std::string primary = text(raw.fields, "primarycolour");
    if (!primary.empty()) out.primary_colour = primary;
    const std::string secondary = text(raw.fields, "secondarycolour");
    if (!secondary.empty()) out.secondary_colour = secondary;
    const std::string outline_colour = text(raw.fields, "outlinecolour");
    if (!outline_colour.empty()) out.outline_colour = outline_colour;
    const std::string back_colour = text(raw.fields, "backcolour");
    if (!back_colour.empty()) out.back_colour = back_colour;
    if (!text(raw.fields, "outline").empty()) out.outline = number(raw.fields, "outline");
    if (!text(raw.fields, "shadow").empty()) out.shadow = number(raw.fields, "shadow");
    if (!text(raw.fields, "marginl").empty()) out.margin_l = integer(raw.fields, "marginl");
    if (!text(raw.fields, "marginr").empty()) out.margin_r = integer(raw.fields, "marginr");
    if (!text(raw.fields, "marginv").empty()) out.margin_v = integer(raw.fields, "marginv");
    if (!text(raw.fields, "alignment").empty()) out.alignment = integer(raw.fields, "alignment", 2);
    if (!text(raw.fields, "bold").empty()) out.bold = integer(raw.fields, "bold");
    if (!text(raw.fields, "italic").empty()) out.italic = integer(raw.fields, "italic");
    if (!text(raw.fields, "scalex").empty()) out.scale_x = number(raw.fields, "scalex", 100.0);
    if (!text(raw.fields, "scaley").empty()) out.scale_y = number(raw.fields, "scaley", 100.0);
    if (!text(raw.fields, "spacing").empty()) out.spacing = number(raw.fields, "spacing");
    if (!text(raw.fields, "angle").empty()) out.angle = number(raw.fields, "angle");
    if (!text(raw.fields, "borderstyle").empty()) out.border_style = integer(raw.fields, "borderstyle", 1);
    return out;
}

void merge_values(MergedStyle &target, const StyleValues &values) {
    if (!values.font_name.empty()) target.font_name = values.font_name;
    if (values.font_size) target.font_size = *values.font_size;
    if (values.primary_colour) target.primary_colour = *values.primary_colour;
    if (values.secondary_colour) target.secondary_colour = *values.secondary_colour;
    if (values.outline_colour) target.outline_colour = *values.outline_colour;
    if (values.back_colour) target.back_colour = *values.back_colour;
    if (values.outline) target.outline = *values.outline;
    if (values.shadow) target.shadow = *values.shadow;
    if (values.margin_l) target.margin_l = *values.margin_l;
    if (values.margin_r) target.margin_r = *values.margin_r;
    if (values.margin_v) target.margin_v = *values.margin_v;
    if (values.alignment) target.alignment = *values.alignment;
    if (values.bold) target.bold = *values.bold;
    if (values.italic) target.italic = *values.italic;
    if (values.scale_x) target.scale_x = *values.scale_x;
    if (values.scale_y) target.scale_y = *values.scale_y;
    if (values.spacing) target.spacing = *values.spacing;
    if (values.angle) target.angle = *values.angle;
    if (values.border_style) target.border_style = *values.border_style;
}

}  // namespace

StyleCatalog resolve_styles(const AssFile &file, int target_width, int target_height) {
    StyleCatalog catalog;
    std::map<std::string, Definition> definitions;
    for (const auto &raw : file.styles) {
        const std::string key = key_of(raw.name);
        if (key.empty()) continue;
        ++catalog.definitions;
        if (definitions.count(key)) {
            ++catalog.duplicate_definitions;
            catalog.diagnostics.push_back({"warning", file.path, raw.line, "duplicate_style",
                                           "Style '" + raw.name + "' redefined; later definition wins"});
        }
        Definition definition;
        definition.raw = raw;
        definition.values = parse_values(raw);
        definition.parent_key = key_of(definition.values.parent);
        if (definition.parent_key.empty() && key != "default") definition.parent_key = "default";
        definitions[key] = std::move(definition);
    }

    std::function<void(const std::string &)> visit = [&](const std::string &key) {
        Definition &node = definitions.at(key);
        node.state = 1;
        if (!node.parent_key.empty() && definitions.count(node.parent_key)) {
            Definition &parent = definitions.at(node.parent_key);
            if (parent.state == 1) {
                node.cycle = true;
                parent.cycle = true;
                catalog.diagnostics.push_back({"error", file.path, node.raw.line, "style_cycle",
                                               "Parent style cycle involving '" + node.raw.name + "'"});
            } else if (parent.state == 0) {
                visit(node.parent_key);
            }
            if (parent.cycle) node.cycle = true;
        }
        node.state = 2;
    };
    for (const auto &item : definitions) {
        if (item.second.state == 0) visit(item.first);
    }

    std::map<std::string, ResolvedStyle> memo;
    std::function<const ResolvedStyle &(const std::string &)> resolve =
        [&](const std::string &key) -> const ResolvedStyle & {
        const auto memoized = memo.find(key);
        if (memoized != memo.end()) return memoized->second;
        const Definition &definition = definitions.at(key);
        ResolvedStyle style;
        style.name = definition.raw.name;
        style.parent = definition.values.parent;
        style.cycle = definition.cycle;
        style.valid = !definition.cycle;
        if (definition.cycle) {
            style.chain = {definition.raw.name};
            style.merged = MergedStyle{};
            std::string cursor = definition.parent_key;
            for (std::size_t depth = 0; depth < definitions.size() && definitions.count(cursor); ++depth) {
                const std::string &name = definitions.at(cursor).raw.name;
                style.chain.push_back(name);
                if (cursor == key) break;
                cursor = definitions.at(cursor).parent_key;
            }
            const double y_scale = static_cast<double>(target_height) / file.info.play_res_y;
            const double x_scale = static_cast<double>(target_width) / file.info.play_res_x;
            style.font_size_px = style.merged.font_size * y_scale;
            style.outline_px = style.merged.outline * std::min(x_scale, y_scale);
            return memo.emplace(key, std::move(style)).first->second;
        }
        const std::string parent_key = definition.parent_key;
        const bool parent_exists = !parent_key.empty() && definitions.count(parent_key);
        if (!parent_key.empty() && !parent_exists) {
            style.parent_fallback = true;
            catalog.diagnostics.push_back({"warning", file.path, definition.raw.line, "parent_fallback",
                                           "Parent style '" + definition.values.parent +
                                               "' missing; falling back to Default"});
        }
        MergedStyle merged = MergedStyle{};
        if (parent_exists) {
            const ResolvedStyle &parent = resolve(parent_key);
            merged = parent.merged;
            style.chain = parent.chain;
            if (!parent.valid) style.valid = false;
        } else if (definitions.count("default") && key != "default") {
            const ResolvedStyle &parent = resolve("default");
            merged = parent.merged;
            style.chain = parent.chain;
            if (!parent.valid) style.valid = false;
        } else {
            style.chain.push_back("Default");
        }
        merge_values(merged, definition.values);
        style.merged = merged;
        style.chain.push_back(definition.raw.name);
        const double y_scale = static_cast<double>(target_height) / file.info.play_res_y;
        const double x_scale = static_cast<double>(target_width) / file.info.play_res_x;
        style.font_size_px = style.merged.font_size * y_scale;
        style.outline_px = style.merged.outline * std::min(x_scale, y_scale);
        if (definition.raw.name == "Default" || key == "default") {
            style.chain = {definition.raw.name};
            style.parent_fallback = false;
            style.valid = !definition.cycle;
        }
        return memo.emplace(key, std::move(style)).first->second;
    };

    for (const auto &item : definitions) {
        ResolvedStyle resolved = resolve(item.first);
        if (resolved.parent_fallback) {
            resolved.parent = resolved.parent.empty() ? "Default" : resolved.parent;
        }
        catalog.order.push_back(item.first);
        catalog.resolved[item.first] = std::move(resolved);
    }
    return catalog;
}

}  // namespace subqc
