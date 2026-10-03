package com.carrier.sigstore;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

/**
 * 过滤谓词。多个谓词之间是 AND 关系，会被下推为位图交集。
 *
 * <p>IN 的取值在构造时排序去重，保证同一逻辑谓词永远编译成同一段位图 AND 序列，
 * 从而让过滤结果（以及随后的行序）可复现。
 */
public final class Filter {

    public enum Kind { EQ, IN, RANGE }

    private final Kind kind;
    private final int columnId;
    private final String columnName;
    private final long[] values;   // EQ/IN：排序去重后的取值
    private final long lo;
    private final long hi;
    private final boolean loInclusive;
    private final boolean hiInclusive;

    private Filter(Kind kind, int columnId, String columnName, long[] values,
                   long lo, long hi, boolean loInclusive, boolean hiInclusive) {
        this.kind = kind;
        this.columnId = columnId;
        this.columnName = columnName;
        this.values = values;
        this.lo = lo;
        this.hi = hi;
        this.loInclusive = loInclusive;
        this.hiInclusive = hiInclusive;
    }

    public static Filter eq(Schema schema, String column, long value) {
        int id = schema.columnId(column);
        return new Filter(Kind.EQ, id, column, new long[]{value}, 0, 0, true, true);
    }

    public static Filter in(Schema schema, String column, long... vals) {
        int id = schema.columnId(column);
        long[] sorted = Arrays.copyOf(vals, vals.length);
        Arrays.sort(sorted);
        int n = 0;
        for (int i = 0; i < sorted.length; i++) {
            if (i == 0 || sorted[i] != sorted[i - 1]) sorted[n++] = sorted[i];
        }
        return new Filter(Kind.IN, id, column, Arrays.copyOf(sorted, n), 0, 0, true, true);
    }

    public static Filter range(Schema schema, String column, long lo, long hi) {
        return range(schema, column, lo, hi, true, true);
    }

    public static Filter range(Schema schema, String column, long lo, long hi,
                               boolean loInclusive, boolean hiInclusive) {
        if (lo > hi || (lo == hi && !(loInclusive && hiInclusive))) {
            return in(schema, column);   // 空区间，直接落成空 IN
        }
        int id = schema.columnId(column);
        return new Filter(Kind.RANGE, id, column, null, lo, hi, loInclusive, hiInclusive);
    }

    public Kind kind() {
        return kind;
    }

    public int columnId() {
        return columnId;
    }

    public String columnName() {
        return columnName;
    }

    public long[] values() {
        return values;
    }

    public long lo() {
        return lo;
    }

    public long hi() {
        return hi;
    }

    public boolean loInclusive() {
        return loInclusive;
    }

    public boolean hiInclusive() {
        return hiInclusive;
    }

    /** 确定性字符串形式，用于 EXPLAIN 与测试断言。 */
    @Override
    public String toString() {
        return switch (kind) {
            case EQ -> columnName + " = " + values[0];
            case IN -> columnName + " IN " + Arrays.toString(values);
            case RANGE -> columnName + (loInclusive ? " >= " : " > ") + lo
                    + " AND " + columnName + (hiInclusive ? " <= " : " < ") + hi;
        };
    }

    /** 一组 AND 谓词。保持声明顺序，编译期不会重排。 */
    public static final class And {
        private final Schema schema;
        private final List<Filter> filters = new ArrayList<>();

        public And(Schema schema) {
            this.schema = schema;
        }

        public And eq(String column, long value) {
            filters.add(Filter.eq(schema, column, value));
            return this;
        }

        public And in(String column, long... vals) {
            filters.add(Filter.in(schema, column, vals));
            return this;
        }

        public And range(String column, long lo, long hi) {
            filters.add(Filter.range(schema, column, lo, hi));
            return this;
        }

        public And add(Filter f) {
            filters.add(f);
            return this;
        }

        public List<Filter> filters() {
            return List.copyOf(filters);
        }

        public boolean isEmpty() {
            return filters.isEmpty();
        }
    }
}
