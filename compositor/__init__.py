"""确定性离线帧序列合成器。

模块划分:
- types:     时间码、图层、音频事件与合成结果等领域类型
- timeline:  JSON 时间线解析与校验(整数帧号/采样点统一换算)
- blend:     source-over / add / multiply / screen 四种 RGBA 混合
- renderer:  图层排序、裁剪、缩放旋转与逐帧像素合成
- audio:     确定性 PCM 夹具生成与音频事件采样范围分析
- digest:    像素哈希、透明像素统计与全局摘要
- exporter:  复用 renderer 输出帧摘要(禁止写图像文件)
- cli:       终端入口、JSON 输出与退出码
"""

__version__ = "1.0.0"
