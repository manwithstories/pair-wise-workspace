#include "subqc/event_model.hpp"

#include <algorithm>

#include "subqc/ass_reader.hpp"
#include "subqc/inline_tags.hpp"

namespace subqc {

void build_event_model(AnalyzedFile &file) {
    file.events.clear();
    const auto &catalog = file.catalog;
    for (const auto &event : file.ass.events) {
        std::string base = event.style.empty() ? "Default" : event.style;
        auto found = catalog.resolved.find(lower_copy(base));
        if (found == catalog.resolved.end()) {
            file.ass.diagnostics.push_back({"warning", file.ass.path, event.line, "missing_event_style",
                                            "Event style '" + base + "' missing; using Default"});
            base = "Default";
            found = catalog.resolved.find("default");
        }
        if (found != catalog.resolved.end() && !found->second.valid) {
            file.ass.diagnostics.push_back({"warning", file.ass.path, event.line, "invalid_event_style",
                                            "Event style '" + base + "' is invalid; using Default"});
            base = "Default";
            found = catalog.resolved.find("default");
        }
        if (found != catalog.resolved.end()) base = found->second.name;

        AnalyzedEvent analyzed;
        analyzed.event = event;
        analyzed.base_style = base;
        analyzed.effective_style = base;
        analyzed.inline_tags = analyze_inline_tags(event.text, base, catalog, file.ass.path, event.line);
        for (auto &diagnostic : analyzed.inline_tags.diagnostics) {
            file.ass.diagnostics.push_back(std::move(diagnostic));
        }
        analyzed.inline_tags.diagnostics.clear();
        if (!analyzed.inline_tags.segments.empty())
            analyzed.effective_style = analyzed.inline_tags.segments.back().style;
        file.events.push_back(std::move(analyzed));
    }
    std::stable_sort(file.events.begin(), file.events.end(), [](const AnalyzedEvent &left,
                                                               const AnalyzedEvent &right) {
        if (left.event.start_ms != right.event.start_ms) return left.event.start_ms < right.event.start_ms;
        if (left.event.layer != right.event.layer) return left.event.layer < right.event.layer;
        return left.event.input_index < right.event.input_index;
    });
}

}  // namespace subqc
