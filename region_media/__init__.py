"""根目录包垫片。

验收命令直接以 ``python -m region_media.cli`` 运行且不安装项目本身。
真实实现全部位于 ``src/region_media``，这里在导入时把 ``src`` 加入
``sys.path`` 并扩展本包的搜索路径，使 ``region_media.cli`` 等子模块
能被正常定位，同时不要求把项目 pip install 进虚拟环境。
"""

import os
import sys

_SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                    "src")
if _SRC not in sys.path:
    sys.path.insert(0, _SRC)

from pkgutil import extend_path  # noqa: E402

__path__ = extend_path(__path__, __name__)
