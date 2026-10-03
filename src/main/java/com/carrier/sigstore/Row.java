package com.carrier.sigstore;

import java.util.Arrays;

/**
 * 一行话单。内部一律用 long[] 承载，避免装箱，也避免任何哈希容器参与输出序。
 *
 * <p>行一旦提交即不可变（append-only）。这是整套无锁读的基础：全局 rowId 在
 * <b>提交时</b>按提交序分配，因此 rowId 天然编码了"提交先后"，快照只需一个高水位即可
 * 屏蔽所有后提交的位。
 */
public final class Row {

    private final long[] values;

    private Row(long[] values) {
        this.values = values;
    }

    public static Row of(long... values) {
        return new Row(Arrays.copyOf(values, values.length));
    }

    public static Row ofInts(int... values) {
        long[] v = new long[values.length];
        for (int i = 0; i < values.length; i++) v[i] = values[i];
        return new Row(v);
    }

    public int width() {
        return values.length;
    }

    public long get(int i) {
        return values[i];
    }

    public long[] toArray() {
        return Arrays.copyOf(values, values.length);
    }

    /** 确定性相等：按下标逐列比较，与哈希容器实现无关。 */
    @Override
    public boolean equals(Object o) {
        if (this == o) return true;
        if (!(o instanceof Row other)) return false;
        return Arrays.equals(values, other.values);
    }

    @Override
    public int hashCode() {
        return Arrays.hashCode(values);
    }

    @Override
    public String toString() {
        return "Row" + Arrays.toString(values);
    }
}
