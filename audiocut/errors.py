"""Exit-code-carrying exceptions shared by all audiocut modules."""

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_UNSUPPORTED = 3
EXIT_PROFILE = 4
EXIT_PARTIAL = 5
EXIT_INTERRUPTED = 130


class AudioCutError(Exception):
    """Base error; ``exit_code`` maps to the process exit status."""

    exit_code = EXIT_USAGE

    def __init__(self, message, *, path=None):
        super().__init__(message)
        self.path = path


class UnsupportedFormatError(AudioCutError):
    """Float/compressed WAV or unsupported bit depth (exit 3)."""

    exit_code = EXIT_UNSUPPORTED


class ProfileError(AudioCutError):
    """Calibration profile invalid or mismatched (exit 4)."""

    exit_code = EXIT_PROFILE


class OutputPathError(AudioCutError):
    """Output escapes --out-dir or goes through a symlink (exit 2)."""

    exit_code = EXIT_USAGE
