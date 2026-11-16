"""Robust sliding-window bandwidth estimator.

The estimator is the single component allowed to turn raw
:class:`~abr_engine.models.NetworkSample` values into a bandwidth guess;
the policy only ever consumes its :class:`~abr_engine.models.Estimate`,
so the two concerns stay separable and testable.

Safety contract (see :class:`EstimatorState`):

* fewer than ``min_samples`` observations      -> ``WARMUP`` safe=False
* no observations at all                        -> ``EMPTY``  safe=False
* a non-monotonic timestamp is seen             -> sample rejected,
                                                   state ``REGRESSION``
* every sample in the window has zero throughput -> ``ZERO``   safe=False
* median-based outliers are suppressed, then a recency-weighted mean of
  the survivors is reported with state ``OK``
"""

from __future__ import annotations

from collections import deque

from .models import Estimate, EstimatorState, NetworkSample


class BandwidthEstimator:
    """Stateful, append-only estimator over a bounded sample window."""

    def __init__(
        self,
        window: int = 6,
        min_samples: int = 2,
        outlier_ratio: float = 1.8,
        loss_penalty: float = 2.0,
    ) -> None:
        if window < 1:
            raise ValueError("window must be >= 1")
        if min_samples < 1:
            raise ValueError("min_samples must be >= 1")
        if not 1.0 < outlier_ratio <= 5.0:
            raise ValueError("outlier_ratio must be in (1, 5]")
        if loss_penalty < 0:
            raise ValueError("loss_penalty must be >= 0")
        self.window = window
        self.min_samples = min(min_samples, window)
        self.outlier_ratio = outlier_ratio
        self.loss_penalty = loss_penalty
        self._samples: deque[NetworkSample] = deque(maxlen=window)
        self._last_t: float | None = None
        self._regression_seen = False

    # ------------------------------------------------------------------ #
    # Ingestion
    # ------------------------------------------------------------------ #
    def add(self, sample: NetworkSample) -> bool:
        """Append a sample. Returns False if its timestamp regressed.

        A rejected sample never enters the window; the next
        :meth:`estimate` reports ``REGRESSION`` until the window turns over.
        """
        if self._last_t is not None and sample.t < self._last_t:
            self._regression_seen = True
            return False
        self._last_t = sample.t
        self._samples.append(sample)
        return True

    # ------------------------------------------------------------------ #
    # Estimation
    # ------------------------------------------------------------------ #
    @staticmethod
    def _weighted_mean(values: list[float]) -> float:
        # Linear recency weights: oldest gets 1, newest gets n.
        weights = range(1, len(values) + 1)
        total_weight = sum(weights)
        return sum(w * v for w, v in zip(weights, values)) / total_weight

    def _robust_values(self, values: list[float]) -> list[float]:
        """Drop median-relative outliers without discarding a genuine dip.

        A sustained dip shifts the median itself, so its samples survive;
        only a *minority* of lone spikes/crashes relative to the local
        median are filtered. The minority cap (25% of the window) means a
        cluster of low values — a real capacity change — is never removed
        just because it sits below the median band.
        """
        if len(values) <= 2:
            return values
        ordered = sorted(values)
        mid = len(ordered) // 2
        median = ordered[mid] if len(ordered) % 2 else (
            (ordered[mid - 1] + ordered[mid]) / 2.0
        )
        if median <= 0.0:
            # Zeros are a real signal (a dying link), never filter them out.
            return values
        low = median / self.outlier_ratio
        high = median * self.outlier_ratio
        candidates = [v for v in values if not (low <= v <= high)]
        # Keep the bulk of the window; suppress only sparse outliers.
        max_outliers = max(1, len(values) // 4)
        if len(candidates) > max_outliers:
            return values
        robust = [v for v in values if low <= v <= high]
        # Guard against a pathological window where the band removes
        # everything: fall back to the full window rather than crash.
        return robust if robust else values

    def estimate(self) -> Estimate:
        """Compute the current estimate and its safety state."""
        if not self._samples:
            return Estimate(0.0, EstimatorState.EMPTY, 0, 0.0, 0.0)

        latest = self._samples[-1]
        if self._regression_seen:
            return Estimate(0.0, EstimatorState.REGRESSION,
                            len(self._samples), latest.loss, latest.rtt)

        throughput = [s.throughput for s in self._samples]
        if len(throughput) < self.min_samples:
            return Estimate(0.0, EstimatorState.WARMUP,
                            len(throughput), latest.loss, latest.rtt)

        if all(v == 0.0 for v in throughput):
            return Estimate(0.0, EstimatorState.ZERO,
                            len(throughput), latest.loss, latest.rtt)

        robust = self._robust_values(throughput)
        weighted = self._weighted_mean(robust)
        # Observed loss directly erodes usable goodput; RTT is reported for
        # diagnostics and policy thresholds rather than folded in twice.
        bandwidth = weighted * max(0.0, 1.0 - self.loss_penalty * latest.loss)
        return Estimate(
            bandwidth_kbps=max(0.0, bandwidth),
            state=EstimatorState.OK,
            samples_used=len(robust),
            loss=latest.loss,
            rtt=latest.rtt,
        )

    def reset(self) -> None:
        self._samples.clear()
        self._last_t = None
        self._regression_seen = False
