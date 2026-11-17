"""退出码与领域异常。"""

from __future__ import annotations


class ExitCode:
    OK = 0
    BUSINESS_ERROR = 2      # 业务校验错误（素材缺失、时长不足、零速、循环引用等）
    FORMAT_ERROR = 3        # 输入格式或参数错误
    WRITE_ERROR = 4         # 输出写入失败


class FormatError(Exception):
    """交换格式结构错误，必须携带行号。"""

    def __init__(self, message: str, line: int = 0, source: str = "") -> None:
        self.line = line
        self.source = source
        where = f"{source}:" if source else ""
        super().__init__(f"{where}第 {line} 行: {message}" if line else message)
