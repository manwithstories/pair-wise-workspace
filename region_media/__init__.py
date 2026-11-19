"""根目录命名空间垫片。

验收命令直接以 ``python -m region_media.cli`` 运行且不安装项目本身，
这里通过 pkgutil 路径扩展把 ``src/region_media`` 加入同一包的搜索路径
（经典 pkgutil namespace 写法），真实实现全部位于 ``src/`` 下。
"""

from pkgutil import extend_path

__path__ = extend_path(__path__, __name__)
