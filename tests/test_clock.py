"""共享时钟：有理数换算、回退分段与编码器重启测试。"""

from fractions import Fraction

from mediasync.clock import MediaClock, TimelineBuilder
from mediasync.errors import ErrorBook, ErrorCode


def test_clock_rational_conversions_exact():
    clock = MediaClock(30000)
    assert clock.tick_to_ms(1500) == Fraction(50)
    assert clock.samples_to_ms(240, 48000) == Fraction(5)
    assert clock.ms_to_samples(Fraction(1000), 48000) == 48000
    # 30fps 网格对齐：123ms 向下对齐到 100ms 网格。
    assert clock.align_ms(Fraction(123), 100) == Fraction(100)


def test_timeline_monotonic_single_epoch():
    book = ErrorBook()
    builder = TimelineBuilder(MediaClock(1000), "v.h264", book)
    for tick in (0, 33, 67, 100):
        builder.add(tick)

    assert builder.epoch_count == 1
    assert [p.source_tick for p in builder.points] == [0, 33, 67, 100]
    assert builder.points[1].continuous_ms == Fraction(33)
    assert builder.duration_ms == Fraction(100)
    assert not book.any_fatal()


def test_timeline_backward_timestamp_opens_epoch_keeps_source_ticks():
    book = ErrorBook()
    builder = TimelineBuilder(MediaClock(1000), "v.h265", book)
    builder.add(0)
    builder.add(40)
    builder.add(10)  # 回退
    builder.add(30)

    assert builder.epoch_count == 2
    backward = book.by_code(ErrorCode.TIMESTAMP_BACKWARD)
    assert len(backward) == 1 and backward[0].fatal is False
    # 源 tick 原样保留。
    assert [p.source_tick for p in builder.points] == [0, 40, 10, 30]
    # 新 epoch 从旧末端 40ms 接续，间隔不被改写。
    second = [p for p in builder.points if p.epoch == 1]
    assert second[0].continuous_ms == Fraction(40)
    assert second[1].continuous_ms == Fraction(60)
    assert builder.duration_ms == Fraction(60)
    epochs = builder.epochs()
    assert epochs == [(0, Fraction(0), Fraction(40)), (1, Fraction(40), Fraction(60))]


def test_timeline_encoder_restart_recorded():
    book = ErrorBook()
    builder = TimelineBuilder(MediaClock(90000), "v.av1", book)
    builder.add(0)
    builder.mark_restart(at_tick=3000)
    builder.add(0)
    builder.add(3000)

    assert builder.epoch_count == 2
    assert book.has(ErrorCode.ENCODER_RESTART)
    restart = book.by_code(ErrorCode.ENCODER_RESTART)[0]
    assert restart.detail == {"tick": 3000, "new_epoch": 1}
    # 重启后首点从旧末端继续，源 tick=0 保留。
    assert builder.points[1].source_tick == 0
    assert builder.points[1].epoch == 1
    assert builder.points[2].source_tick == 3000
