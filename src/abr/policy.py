"""码率决策策略：在可行性硬约束下权衡切换收益与风险。

决策输入为估计带宽、播放缓冲快照、带宽趋势与候选档位表。核心硬约束：
任何“预计无法在缓冲耗尽前下载完成”的档位一律禁止选择（可行性）。
在此之上，升档要求连续安全余量，降档快速触发但受最低质量与冷却约束，
下溢保护期间强制最低可行档。
"""

from __future__ import annotations

from dataclasses import dataclass

from .types import (
    BandwidthEstimate,
    BitrateLevel,
    DecisionKind,
    DecisionResult,
    PlaybackState,
    SwitchReason,
)


@dataclass(frozen=True, slots=True)
class PolicyConfig:
    """策略参数。"""

    safety_margin: float = 1.3       # 升档要求估计带宽 >= 档码率 * margin
    downshift_margin: float = 1.05   # 可行性边界：估计带宽需略高于档码率
    confirm_segments: int = 3        # 升档需连续满足余量的段数
    cooldown_segments: int = 2       # 切换后的冷却段数（紧急降档豁免）
    recovery_hysteresis_s: float = 6.0  # 保护解除后抑制升档的缓冲滞后窗口
    trend_weight: float = 0.15       # 趋势对有效带宽的修正强度

    def validate(self) -> None:
        if self.safety_margin < 1.0:
            raise ValueError("safety_margin 必须 >= 1.0")
        if self.downshift_margin < 1.0:
            raise ValueError("downshift_margin 必须 >= 1.0")
        if self.confirm_segments < 1:
            raise ValueError("confirm_segments 至少为 1")
        if self.cooldown_segments < 0:
            raise ValueError("cooldown_segments 不能为负")


@dataclass(frozen=True, slots=True)
class PolicyContext:
    """单次决策的只读上下文。"""

    segment_index: int
    levels: tuple[BitrateLevel, ...]
    current_index: int
    estimate: BandwidthEstimate
    playback: PlaybackState
    consecutive_safe: int   # 已连续满足升档余量的段数（控制器维护）
    cooldown_remaining: int  # 剩余冷却段数
    segment_duration_s: float


class ABRPolicy:
    """无状态选档逻辑；滞后/冷却计数由控制器持有并经上下文传入。"""

    def __init__(self, config: PolicyConfig | None = None):
        self._cfg = config or PolicyConfig()
        self._cfg.validate()

    # ------------------------------------------------------------------
    def decide(self, ctx: PolicyContext) -> DecisionResult:
        cfg = self._cfg
        levels = ctx.levels
        cur = ctx.current_index
        est = ctx.estimate
        pb = ctx.playback

        effective_bw = self._effective_bandwidth(est)
        feasible = self._feasible_levels(ctx, effective_bw)
        highest_feasible = max(feasible) if feasible else 0
        pred = self._predict_seconds(levels[cur], effective_bw, ctx.segment_duration_s)
        margin = (effective_bw / levels[cur].bitrate_kbps
                  if levels[cur].bitrate_kbps > 0 else 0.0)

        # 1) 下溢保护：已在保护态，或缓冲已跌破阈值 -> 强制最低可行档
        if pb.in_protection or pb.buffer_seconds <= pb.underflow_threshold_s:
            chosen = min(cur, highest_feasible) if cur in feasible else highest_feasible
            return self._result(ctx, chosen, effective_bw,
                                SwitchReason.UNDERFLOW_PROTECTION, pred, margin,
                                cooldown_applied=False)

        # 2) 非保护态但当前档已不可行（带宽/时间硬约束）-> 紧急降档，豁免冷却
        if cur not in feasible:
            return self._result(ctx, highest_feasible, effective_bw,
                                SwitchReason.FEASIBILITY, pred, margin,
                                cooldown_applied=False)

        # 3) 升档评估：连续余量 + 冷却 + 恢复滞后
        target = self._upshift_target(ctx, feasible, effective_bw)
        if target > cur:
            in_recovery = (not pb.in_protection
                           and pb.buffer_seconds < pb.underflow_threshold_s
                           + cfg.recovery_hysteresis_s)
            enough_confirm = ctx.consecutive_safe + 1 >= cfg.confirm_segments
            if enough_confirm and ctx.cooldown_remaining == 0 and not in_recovery:
                return self._result(ctx, target, effective_bw,
                                    SwitchReason.OPPORTUNISTIC, pred, margin,
                                    cooldown_applied=False)
            # 条件不足：记录一次风险拒绝（仍保持当前档）
            return self._result(ctx, cur, effective_bw, SwitchReason.NONE,
                                pred, margin, risk_rejected=True,
                                cooldown_applied=ctx.cooldown_remaining > 0)

        # 4) 无档可升或冷却中：保持
        return self._result(ctx, cur, effective_bw, SwitchReason.NONE,
                            pred, margin,
                            cooldown_applied=ctx.cooldown_remaining > 0)

    # ------------------------------------------------------------------
    def _effective_bandwidth(self, est: BandwidthEstimate) -> float:
        """结合趋势修正的有效带宽；无样本时返回 0（触发保守降档）。"""
        if est.sample_count == 0:
            return 0.0
        # 趋势按一个展望步长加权，限幅在 ±50% 估计值，避免外推失控
        adjustment = est.trend_kbps_per_sample * self._cfg.trend_weight
        cap = est.estimated_kbps * 0.5
        adjustment = max(-cap, min(cap, adjustment))
        return max(0.0, est.estimated_kbps + adjustment)

    def _feasible_levels(self, ctx: PolicyContext, effective_bw: float) -> list[int]:
        """返回所有预计可在缓冲耗尽前完成下载的档位索引。

        判据：预计下载时间 <= 当前缓冲 + 一个分段时长（流水线前瞻），
        且有效带宽满足该档的最低可行性余量。
        """
        pb = ctx.playback
        budget = pb.buffer_seconds + ctx.segment_duration_s
        out: list[int] = []
        for lvl in ctx.levels:
            pred = self._predict_seconds(lvl, effective_bw, ctx.segment_duration_s)
            bw_ok = effective_bw >= lvl.bitrate_kbps * self._cfg.downshift_margin
            time_ok = pred <= budget
            if bw_ok and time_ok:
                out.append(lvl.index)
        return out

    @staticmethod
    def _predict_seconds(lvl: BitrateLevel, effective_bw: float,
                         segment_duration_s: float) -> float:
        """预计下载耗时 = 分段比特 / 有效带宽；零带宽给出无穷大。"""
        if effective_bw <= 0.0:
            return float("inf")
        bits = lvl.bitrate_kbps * segment_duration_s  # kbit
        return bits / effective_bw                     # 秒

    def _upshift_target(self, ctx: PolicyContext, feasible: list[int],
                        effective_bw: float) -> int:
        """在可行档中选出满足安全余量的最高档。"""
        target = ctx.current_index
        for idx in feasible:
            if idx <= ctx.current_index:
                continue
            lvl = ctx.levels[idx]
            if effective_bw >= lvl.bitrate_kbps * self._cfg.safety_margin:
                target = idx
        return target

    def _result(self, ctx: PolicyContext, chosen: int, effective_bw: float,
                reason: SwitchReason, pred_cur: float, margin_cur: float,
                risk_rejected: bool = False,
                cooldown_applied: bool = False) -> DecisionResult:
        chosen_lvl = ctx.levels[chosen]
        pred = self._predict_seconds(chosen_lvl, effective_bw,
                                     ctx.segment_duration_s)
        if chosen == ctx.current_index:
            kind = DecisionKind.HOLD
        elif chosen > ctx.current_index:
            kind = DecisionKind.UPSHIFT
        else:
            kind = DecisionKind.DOWNSHIFT
        return DecisionResult(
            segment_index=ctx.segment_index,
            chosen_index=chosen,
            previous_index=ctx.current_index,
            kind=kind,
            reason=reason,
            predicted_download_s=pred if kind != DecisionKind.HOLD else pred_cur,
            safety_margin=(effective_bw / chosen_lvl.bitrate_kbps
                           if chosen_lvl.bitrate_kbps > 0 else 0.0),
            risk_rejected=risk_rejected,
            cooldown_applied=cooldown_applied,
        )
