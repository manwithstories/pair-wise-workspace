#include "segments.hpp"

void Segmenter::push(std::size_t index, double intensity) {
    if (open_) {
        if (intensity >= p_.exit) {
            last_high_ = index;
            gap_used_ = 0;
        } else {
            ++gap_used_;
            if (gap_used_ > p_.gap)
                close_open();
        }
    }
    if (!open_) {
        if (intensity >= p_.enter) {
            if (run_len_ == 0)
                run_start_ = index;
            ++run_len_;
            if (run_len_ >= p_.min_frames) {
                open_ = true;
                begin_ = run_start_;
                last_high_ = index;
                gap_used_ = 0;
                run_len_ = 0;
            }
        } else {
            run_len_ = 0;
        }
    }
}

void Segmenter::cut() {
    if (open_)
        close_open();
    run_len_ = 0;
}

void Segmenter::close_open() {
    segs_.push_back(Segment{begin_, last_high_ + 1});
    open_ = false;
}
