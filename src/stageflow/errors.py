"""Error model for stageflow.

Every failure raised inside the pipeline is a StageflowError carrying:
  - error_code: stable machine-readable code (printed as error_code=...)
  - stage:      pipeline stage that raised (timeline/ingest/clock/mix/render/manifest)
  - track:      track id when the failure is track-scoped, else None
  - sample_offset: absolute sample position (delivery-rate ticks) when known
  - path:       input file involved, else None

Exit-code mapping (see cli.py):
  2  parameter or manifest errors        (PARAM_* / MANIFEST_*)
  3  media corruption / timeline mismatch (WAV_* / NONFINITE_* / LAYOUT_* / TIMELINE_*)
  4  interrupted and could not resume     (INTERRUPTED)
"""

from __future__ import annotations


class StageflowError(Exception):
    exit_code = 3  # default: media corruption / timeline inconsistency

    def __init__(
        self,
        error_code: str,
        message: str,
        *,
        stage: str,
        track: str | None = None,
        sample_offset: int | None = None,
        path: str | None = None,
    ) -> None:
        super().__init__(message)
        self.error_code = error_code
        self.message = message
        self.stage = stage
        self.track = track
        self.sample_offset = sample_offset
        self.path = path

    def locate_fields(self) -> dict[str, object]:
        """Fields printed so an operator can locate the failure."""
        out: dict[str, object] = {"error_code": self.error_code, "stage": self.stage}
        if self.track is not None:
            out["track"] = self.track
        if self.sample_offset is not None:
            out["sample_offset"] = self.sample_offset
        if self.path is not None:
            out["path"] = self.path
        out["detail"] = self.message
        return out


class ManifestError(StageflowError):
    """Bad CLI parameters or invalid timeline manifest."""

    exit_code = 2

    def __init__(self, error_code: str, message: str, **kw: object) -> None:
        kw.setdefault("stage", "timeline")
        super().__init__(error_code, message, **kw)  # type: ignore[arg-type]


class MediaError(StageflowError):
    """Corrupt or unsupported media."""

    exit_code = 3


class InterruptedError_(StageflowError):
    """Processing interrupted and resume was not possible."""

    exit_code = 4

    def __init__(self, message: str, **kw: object) -> None:
        kw.setdefault("stage", "pipeline")
        super().__init__("INTERRUPTED", message, **kw)  # type: ignore[arg-type]
