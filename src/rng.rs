//! Deterministic, dependency-free pseudo random generator (LCG, Numerical
//! Recipes constants). The entire pipeline output is a pure function of the
//! CLI `--seed`, so fixed seeds always reproduce identical logs and metrics.

#[derive(Debug, Clone)]
pub struct Rng {
    state: u64,
}

impl Rng {
    pub fn new(seed: u64) -> Self {
        Self {
            state: if seed == 0 { 0x9e3779b97f4a7c15 } else { seed },
        }
    }

    pub fn next_u64(&mut self) -> u64 {
        // 64-bit LCG
        self.state = self
            .state
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
        self.state
    }

    pub fn next_below(&mut self, bound: u64) -> u64 {
        if bound == 0 {
            return 0;
        }
        self.next_u64() % bound
    }

    pub fn next_range(&mut self, lo: u64, hi: u64) -> u64 {
        if hi <= lo {
            lo
        } else {
            lo + self.next_below(hi - lo)
        }
    }

    pub fn choose<'a, T>(&mut self, items: &'a [T]) -> &'a T {
        let idx = self.next_below(items.len() as u64) as usize;
        &items[idx]
    }
}
