#include "allocator.hpp"

#include <algorithm>
#include <cmath>
#include <numeric>
#include <stdexcept>

std::vector<uint64_t> allocate_bits(const std::vector<double>& intensities,
                                    uint64_t budget) {
    const std::size_t n = intensities.size();
    if (n == 0) {
        if (budget != 0)
            throw std::invalid_argument(
                "cannot allocate a nonzero budget over zero frames");
        return {};
    }
    std::vector<long double> weight(n);
    long double total = 0.0L;
    for (std::size_t i = 0; i < n; ++i) {
        weight[i] = 1.0L + 3.0L * static_cast<long double>(intensities[i]);
        total += weight[i];
    }

    std::vector<uint64_t> out(n, 0);
    std::vector<long double> remainder(n, 0.0L);
    uint64_t assigned = 0;
    for (std::size_t i = 0; i < n; ++i) {
        const long double ideal =
            static_cast<long double>(budget) * weight[i] / total;
        const long double base = std::floor(ideal);
        out[i] = static_cast<uint64_t>(base);
        remainder[i] = ideal - base;
        assigned += out[i];
    }

    // Leftover units go to the largest remainders; ties by lower index.
    // The leftover is always < n because sum(remainder) < n.
    const uint64_t left = budget - assigned;
    std::vector<std::size_t> order(n);
    std::iota(order.begin(), order.end(), std::size_t{0});
    std::stable_sort(order.begin(), order.end(),
                     [&](std::size_t a, std::size_t b) {
                         return remainder[a] > remainder[b];
                     });
    for (uint64_t k = 0; k < left; ++k)
        ++out[order[k]];
    return out;
}
