"""色彩元数据汇总与裁决。

现场多机位的 primaries/transfer/space/range 可能缺失或互相冲突。
按清单逐轨声明的策略裁决：inherit（继承参考/默认）、override（明确覆盖）、
error（冲突即报错）。输出逐轨裁决依据，供报告解释每个字段从何而来。
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .model import (ColorDecl, ColorStrategy, ErrorCategory, Issue,
                    Manifest, PipelineError, TrackDecl, TrackKind)

COLOR_FIELDS = ("primaries", "transfer", "space", "range")


@dataclass(slots=True)
class FieldDecision:
    field: str
    value: str | None
    source: str                 # self / default / reference / override
    reason: str


@dataclass(slots=True)
class TrackColorVerdict:
    track_id: str
    decisions: list[FieldDecision] = field(default_factory=list)
    conflicts: list[Issue] = field(default_factory=list)

    def merged(self) -> dict[str, str | None]:
        return {d.field: d.value for d in self.decisions}


def _decl_get(decl: ColorDecl, name: str) -> str | None:
    return getattr(decl, name)


def arbitrate_track(track: TrackDecl, manifest: Manifest,
                    reference: TrackDecl | None) -> TrackColorVerdict:
    verdict = TrackColorVerdict(track_id=track.track_id)
    strategy = track.color.strategy
    inherited = reference.color if reference else manifest.color_default

    for name in COLOR_FIELDS:
        own = _decl_get(track.color, name)
        parent = _decl_get(inherited, name)
        default = _decl_get(manifest.color_default, name)

        if strategy is ColorStrategy.OVERRIDE and own is not None:
            verdict.decisions.append(FieldDecision(
                name, own, "override", f"策略 override，采用本轨显式 {name}"))
            continue

        if own is not None and parent is not None and own != parent:
            # 冲突：本轨与继承源不一致。
            issue = Issue(
                ErrorCategory.COLOR_CONFLICT, track.track_id,
                f"色彩冲突 {name}: 本轨={own} 继承源={parent} "
                f"策略={strategy.value}",
                detail={"field": name, "own": own, "inherited": parent,
                        "strategy": strategy.value})
            if strategy is ColorStrategy.ERROR:
                verdict.conflicts.append(issue)
                raise PipelineError(
                    ErrorCategory.COLOR_CONFLICT, issue.message,
                    track=track.track_id)
            # inherit 下以参考/默认为准，并保留冲突证据。
            verdict.conflicts.append(issue)
            chosen, src, reason = parent, "reference" if reference else "default", \
                "策略 inherit，冲突时采用继承源"
        elif own is not None:
            chosen, src, reason = own, "self", f"本轨显式声明 {name}"
        elif parent is not None:
            chosen, src, reason = parent, \
                "reference" if reference else "default", \
                f"本轨缺失 {name}，继承{ '参考轨' if reference else '默认'}"
        elif default is not None:
            chosen, src, reason = default, "default", f"回退到清单默认 {name}"
        else:
            chosen, src, reason = None, "none", f"{name} 全链路缺失，保持未知"
        verdict.decisions.append(FieldDecision(name, chosen, src, reason))
    return verdict


def arbitrate_all(manifest: Manifest) -> dict[str, TrackColorVerdict]:
    """对全部轨道裁决；视频轨以 reference_video 为继承源。"""
    ref = None
    if manifest.reference_video:
        ref = manifest.tracks.get(manifest.reference_video)
    verdicts: dict[str, TrackColorVerdict] = {}
    for tid, tr in manifest.tracks.items():
        if tr.kind is TrackKind.VIDEO:
            source = ref if ref is not None and tid != manifest.reference_video \
                else None
            verdicts[tid] = arbitrate_track(tr, manifest, source)
        else:
            # 音频轨无色彩概念，记录空裁决，保持图结构一致。
            verdicts[tid] = TrackColorVerdict(track_id=tid)
    return verdicts
