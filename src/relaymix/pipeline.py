"""作业编排：清单校验 -> 输入指纹 -> 时间线合成 -> 离屏导出 -> 检查点。

render 与 resume 共用同一条幂等管线：已提交窗口由索引判定并跳过，
恢复时强制重验清单、输入块哈希、算法版本与参数指纹。
"""

import hashlib
import json
from pathlib import Path

from . import ALGORITHM_VERSION
from .checkpoint import CheckpointStore
from .errors import CheckpointError, MediaError
from .frame_export import assert_fingerprints, export_timeline
from .manifest import Manifest, parse_manifest
from .timeline import build_timeline, parameter_fingerprint
from .wav_reader import read_block


def compute_input_fingerprint(manifest: Manifest, base_dir: Path) -> str:
    """对清单引用的全部源块按 (track, order) 聚合内容哈希。"""
    h = hashlib.sha256()
    for track in manifest.tracks:
        for spec in track.blocks:
            path = (base_dir / spec.path).resolve()
            block_hash = hashlib.sha256(path.read_bytes()).hexdigest()
            h.update(
                json.dumps(
                    {
                        "track": track.name,
                        "block_id": spec.block_id,
                        "order": spec.order,
                        "content_hash": block_hash,
                    },
                    sort_keys=True,
                ).encode()
            )
    return h.hexdigest()


def _make_loader(base_dir: Path):
    def loader(spec):
        return read_block(
            base_dir / spec.path, spec.block_id,
            pcm_rate=spec.pcm_rate, pcm_channels=spec.pcm_channels,
            pcm_width=spec.pcm_width,
        )

    return loader


def run_job(root: str | Path, manifest_path: str | Path, *,
            interrupt_after: int | None = None, is_resume: bool = False) -> dict:
    """执行（或恢复）一个作业，返回 JSON 可序列化的摘要。"""
    root = Path(root)
    manifest_path = Path(manifest_path)
    manifest = parse_manifest(manifest_path)
    base_dir = manifest_path.parent
    input_fp = compute_input_fingerprint(manifest, base_dir)
    params_fp = parameter_fingerprint(manifest)

    with CheckpointStore(root) as store:
        total_windows = (
            manifest.output_length + manifest.frame_size - 1
        ) // manifest.frame_size
        if store.meta_path.exists():
            store.validate_run(
                manifest_json=json.dumps(manifest.raw, sort_keys=True),
                input_fingerprint=input_fp,
                params_fingerprint=params_fp,
                algo_version=ALGORITHM_VERSION,
            )
            if is_resume:
                meta = store.read_run_meta()
                meta["resume_count"] = int(meta.get("resume_count", 0)) + 1
                tmp = store.meta_path.with_suffix(".json.tmp")
                tmp.write_text(json.dumps(meta, sort_keys=True), encoding="utf-8")
                tmp.replace(store.meta_path)
        else:
            store.write_run_meta(
                manifest_json=json.dumps(manifest.raw, sort_keys=True),
                input_fingerprint=input_fp,
                params_fingerprint=params_fp,
                algo_version=ALGORITHM_VERSION,
                total_chunks=total_windows,
            )

        timeline = build_timeline(manifest, loader=_make_loader(base_dir))
        assert_fingerprints(store, params_fp, ALGORITHM_VERSION)
        export = export_timeline(
            timeline, store,
            algo_version=ALGORITHM_VERSION,
            interrupt_after=interrupt_after,
        )
        resume_count = int(store.read_run_meta().get("resume_count", 0))

    return {
        "status": "interrupted" if not export["finished"] else "ok",
        "root": str(root),
        "total_chunks": export["total_chunks"],
        "committed_now": export["committed_now"],
        "resumed_chunks": export["resumed_chunks"],
        "finished": export["finished"],
        "deduped_blocks": timeline.deduped_blocks,
        "filled_blocks": [
            {
                "track": s.track,
                "source_block": s.block_id,
                "byte_offset": s.byte_offset,
                "reason": s.fill_reason,
                "sample_range": [s.master_start, s.master_end],
            }
            for s in timeline.filled_spans
        ],
        "track_errors": [e.__dict__ for e in timeline.track_errors],
        "params_fingerprint": params_fp,
        "input_fingerprint": input_fp,
        "resume_count": resume_count,
    }
