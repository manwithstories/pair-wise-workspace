"""Error types shared by the CLI, timeline, exporter, and audio modules."""


class TlcompError(Exception):
    """Base error carrying the process exit code."""

    exit_code = 1
    kind = "error"


class UsageError(TlcompError):
    exit_code = 2
    kind = "usage"


class ProjectError(TlcompError):
    exit_code = 2
    kind = "project"


class AssetError(TlcompError):
    exit_code = 3
    kind = "asset"


class ManifestError(TlcompError):
    exit_code = 1
    kind = "manifest"


class VerifyError(TlcompError):
    exit_code = 1
    kind = "verify"


class ResumeError(TlcompError):
    exit_code = 4
    kind = "resume"
