#pragma once

#include <string>

// Generates the nest-box sample fixture into `out_dir`:
//   frame_000.ppm .. frame_011.ppm  (16x16 grayscale P6)
//   frames.tsv                      (timestamp_us TAB relative path)
//
// Timestamps start at a fixed base and step 100000 us. Frames start
// all-black; the black/white state flips at indices 3, 4, 5, 9 and 10,
// yielding five intensity-1 pairs and six intensity-0 pairs.
// Throws std::runtime_error on I/O failure or a non-empty target.
void generate_fixture(const std::string& out_dir);
