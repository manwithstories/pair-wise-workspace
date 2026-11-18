#pragma once

#include <cstddef>
#include <vector>

// Half-open frame-index interval [begin, end).
struct Segment {
    std::size_t begin = 0;
    std::size_t end = 0;
};

// High-motion segment detector with dual thresholds.
//
//  - A segment opens after `min_frames` consecutive frames with
//    intensity >= `enter`.
//  - While open, frames with intensity >= `exit` are "high" and extend
//    the segment; frames below `exit` consume the gap allowance.
//  - When the low-value gap exceeds `gap` frames, the segment closes,
//    falling back to end just after the last high frame.
//  - cut() settles any open segment immediately (skipped/corrupt frame
//    breaks the chain); finish() does the same at end of input.
class Segmenter {
public:
    struct Params {
        double enter = 0.0;
        double exit = 0.0;
        std::size_t min_frames = 1;
        std::size_t gap = 0;
    };

    explicit Segmenter(Params p) : p_(p) {}

    void push(std::size_t index, double intensity);
    void cut();
    void finish() { cut(); }

    const std::vector<Segment>& segments() const { return segs_; }

private:
    void close_open();

    Params p_;
    std::vector<Segment> segs_;
    bool open_ = false;
    std::size_t begin_ = 0;      // start of the open segment
    std::size_t last_high_ = 0;  // last frame >= exit while open
    std::size_t gap_used_ = 0;   // consecutive frames below exit
    std::size_t run_start_ = 0;  // candidate run (>= enter) start
    std::size_t run_len_ = 0;    // candidate run length
};
