"""固定退出码与异常类型。

退出码约定:
  0 成功
  2 参数或工程错误 (ProjectError)
  3 音频读写错误 (AudioIOError)
  4 混音图无效 (GraphError)
  5 双路径不一致 (DivergenceError)
"""

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_AUDIO_IO = 3
EXIT_GRAPH = 4
EXIT_DIVERGENCE = 5


class MixctlError(Exception):
    """所有带固定退出码错误的基类。"""

    exit_code = EXIT_USAGE


class ProjectError(MixctlError):
    """工程 JSON 参数或结构非法。"""

    exit_code = EXIT_USAGE


class AudioIOError(MixctlError):
    """音频文件缺失、损坏或与声明不符。"""

    exit_code = EXIT_AUDIO_IO


class GraphError(MixctlError):
    """混音 DAG 非法（未知总线、成环等）。"""

    exit_code = EXIT_GRAPH


class DivergenceError(MixctlError):
    """离线与流式渲染结果不一致。"""

    exit_code = EXIT_DIVERGENCE
