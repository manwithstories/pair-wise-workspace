package com.carrier.sigcs;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * 倒排位图索引：每基数列一张「值 -> 位向量」，多列过滤下推为位图 AND。
 *
 * <h2>位图表示</h2>
 * <p>一行一位，位号即行号。列的 distinct 值数 &le; {@link #MAX_BITMAP_CARDINALITY} 时自动建图，
 * 超过则该列标记为未建图，查询时回退顺序扫描 —— 高基数列建图反而更慢且更耗内存。
 *
 * <h2>等值与区间</h2>
 * <ul>
 *   <li><b>等值</b>：直接取该值的位向量，与其余谓词 {@code AND}。</li>
 *   <li><b>区间</b>：数值列在建图时按 <b>值域</b>（不是行号）切成至多 {@link #RANGE_SEGMENTS} 段，
 *       每段一个位向量。区间查询二分定位覆盖段后 {@code OR} 起来。
 *       按值域切分使得区间过滤同样免扫描，且段数有上界，合并成本可控。</li>
 * </ul>
 *
 * <h2>确定性</h2>
 * <p>谓词求交的顺序、结果行号的顺序都是确定的：谓词按「(估计宽度, 列名, 类型)」全序排序后求交，
 * 行号从位图低位到高位展开天然升序。二者都与 {@code HashMap} 的哈希随机化无关。
 */
final class BitmapIndex {

    /** 建图阈值：distinct 值数超过它就不再为该列建位图。 */
    static final int MAX_BITMAP_CARDINALITY = 10_000;

    /** 区间分段数上限。 */
    static final int RANGE_SEGMENTS = 256;

    /** 该列未建图（基数过高或不支持），查询需回退扫描。 */
    static final PostingTable NOT_INDEXED = new PostingTable(0, new TreeMap<>(), false);

    private final int rowCount;

    /** 列名 -> 倒排表。LinkedHashMap 仅用于插入顺序稳定；所有对外遍历都另行排序。 */
    private final Map<String, PostingTable> tables = new LinkedHashMap<>();

    BitmapIndex(int rowCount) {
        this.rowCount = rowCount;
    }

    int rowCount() {
        return rowCount;
    }

    // ------------------------------------------------------------------ 建图

    /**
     * 为一列建图。
     *
     * @param name    列名
     * @param values  每行取值，长度为 rowCount
     * @param numeric 该列是否可按数值序分段（数值列建区间位图）
     * @return 实际生效的倒排表；基数超阈值时返回 {@link #NOT_INDEXED}
     */
    PostingTable buildColumn(String name, long[] values, boolean numeric) {
        TreeMap<Long, long[]> postings = new TreeMap<>();
        int words = wordsFor(rowCount);
        for (int row = 0; row < values.length && row < rowCount; row++) {
            long v = values[row];
            long[] bits = postings.computeIfAbsent(v, k -> new long[words]);
            bits[row >>> 6] |= 1L << (row & 63);
        }
        if (postings.size() > MAX_BITMAP_CARDINALITY) {
            tables.put(name, NOT_INDEXED);
            return NOT_INDEXED;
        }
        PostingTable table = new PostingTable(rowCount, postings, numeric);
        tables.put(name, table);
        return table;
    }

    /** 某列的倒排表；未建图返回 {@link #NOT_INDEXED}。 */
    PostingTable table(String column) {
        return tables.getOrDefault(column, NOT_INDEXED);
    }

    boolean isIndexed(String column) {
        PostingTable t = tables.get(column);
        return t != null && t != NOT_INDEXED;
    }

    /** 已建图的列名，按字典序 —— 稳定输出，便于测试断言。 */
    List<String> indexedColumns() {
        List<String> names = new ArrayList<>();
        for (var e : tables.entrySet()) {
            if (e.getValue() != NOT_INDEXED) {
                names.add(e.getKey());
            }
        }
        names.sort(String::compareTo);
        return names;
    }

    /** 某列基数；未建图返回 -1。 */
    int cardinalityOf(String column) {
        PostingTable t = tables.get(column);
        return (t == null || t == NOT_INDEXED) ? -1 : t.cardinality();
    }

    // ------------------------------------------------------------------ 谓词

    enum Kind { EQUALITY, RANGE }

    /** 一个可下推的过滤谓词。 */
    static final class Predicate {
        final String column;
        final Kind kind;
        final long value;
        final long lo;
        final long hi;

        private Predicate(String column, Kind kind, long value, long lo, long hi) {
            this.column = column;
            this.kind = kind;
            this.value = value;
            this.lo = lo;
            this.hi = hi;
        }

        static Predicate eq(String column, long value) {
            return new Predicate(column, Kind.EQUALITY, value, 0L, 0L);
        }

        /** 闭区间 [lo, hi]。 */
        static Predicate range(String column, long lo, long hi) {
            return new Predicate(column, Kind.RANGE, 0L, lo, hi);
        }

        @Override
        public String toString() {
            return kind == Kind.EQUALITY ? column + "=" + value : column + " in [" + lo + "," + hi + "]";
        }

        /**
         * 求出该谓词的位图；无法下推（列未建图）返回 null。
         */
        long[] evaluate(BitmapIndex idx) {
            PostingTable table = idx.table(column);
            if (table == null || table == NOT_INDEXED) {
                return null;
            }
            return kind == Kind.EQUALITY ? table.bitmapOf(value) : table.rangeBitmaps(lo, hi);
        }
    }

    /**
     * 多谓词求交，返回满足全部条件的行号（严格升序）。
     *
     * <p>返回 {@code null} 表示存在无法下推的谓词，调用方应回退到顺序扫描 ——
     * 这里不返回「空结果」，否则会把「没建图」误判成「查不到」。
     */
    List<Integer> intersect(List<Predicate> predicates) {
        // 全序排序：先求交最窄的谓词以减少位运算，且顺序与输入顺序无关 => 结果确定。
        List<Predicate> ordered = new ArrayList<>(predicates);
        ordered.sort(Comparator.comparingInt((Predicate p) -> p.kind == Kind.EQUALITY ? 0 : 1)
                .thenComparing(p -> p.column)
                .thenComparingLong(p -> p.kind == Kind.EQUALITY ? p.value : p.lo)
                .thenComparingLong(p -> p.kind == Kind.EQUALITY ? p.value : p.hi));

        long[] acc = null;
        for (Predicate p : ordered) {
            long[] bits = p.evaluate(this);
            if (bits == null) {
                return null; // 回退扫描
            }
            acc = (acc == null) ? bits : and(acc, bits);
        }
        if (acc == null) {
            return allRows();
        }
        return rowsOf(acc);
    }

    /** 全集行号（升序）。 */
    List<Integer> allRows() {
        long[] all = new long[wordsFor(rowCount)];
        int n = rowCount;
        int fullWords = n >>> 6;
        for (int i = 0; i < fullWords; i++) {
            all[i] = -1L;
        }
        int rem = n & 63;
        if (rem != 0) {
            all[fullWords] = (1L << rem) - 1;
        }
        return rowsOf(all);
    }

    /** 逐字 AND，长度取较短者。 */
    static long[] and(long[] a, long[] b) {
        int n = Math.min(a.length, b.length);
        long[] out = new long[n];
        for (int i = 0; i < n; i++) {
            out[i] = a[i] & b[i];
        }
        return out;
    }

    /** 逐字 OR。 */
    static long[] or(long[] a, long[] b) {
        int n = Math.max(a.length, b.length);
        long[] out = new long[n];
        System.arraycopy(a, 0, out, 0, a.length);
        for (int i = 0; i < b.length; i++) {
            out[i] |= b[i];
        }
        return out;
    }

    /** 位图 -> 升序行号。用 numberOfTrailingZeros 逐位取，顺序与位图内容唯一对应。 */
    static List<Integer> rowsOf(long[] words) {
        List<Integer> rows = new ArrayList<>();
        for (int w = 0; w < words.length; w++) {
            long word = words[w];
            while (word != 0L) {
                rows.add((w << 6) + Long.numberOfTrailingZeros(word));
                word &= word - 1; // 清除最低位的 1
            }
        }
        return rows;
    }

    static int wordsFor(int rows) {
        return (rows + 63) >>> 6;
    }

    // ------------------------------------------------------------------ 倒排表

    /**
     * 一列的倒排表。
     *
     * <p>{@code postings} 提供等值下推；数值列额外预切 {@link #segments} 提供区间下推。
     * 两份位图共享同一份位数据语义，但查询路径不同：等值 O(1) 取一段，区间 O(命中段数)。
     */
    static final class PostingTable {
        private final int rowCount;
        private final boolean numeric;
        private final TreeMap<Long, long[]> postings;
        private final Segment[] segments;

        PostingTable(int rowCount, TreeMap<Long, long[]> postings, boolean numeric) {
            this.rowCount = rowCount;
            this.postings = postings;
            this.numeric = numeric;
            this.segments = numeric ? buildSegments(postings) : new Segment[0];
        }

        int cardinality() {
            return postings.size();
        }

        int segmentCount() {
            return segments.length;
        }

        /** 等值位图；值不存在返回长度 0 的数组（与「空结果」区分由调用方按需处理）。 */
        long[] bitmapOf(long value) {
            long[] bits = postings.get(value);
            return bits == null ? new long[0] : bits;
        }

        boolean contains(long value) {
            return postings.containsKey(value);
        }

        /**
         * 区间 [lo,hi] 的位图：二分找出与 [lo,hi] 相交的段，逐段 OR。
         * 段按值域升序且互不重叠，因此覆盖是精确的。
         */
        long[] rangeBitmaps(long lo, long hi) {
            if (lo > hi || segments.length == 0) {
                return new long[0];
            }
            long[] out = new long[0];
            // 第一个 segMax >= lo 的段
            int start = lowerBoundByMax(lo);
            for (int i = start; i < segments.length && segments[i].minValue <= hi; i++) {
                out = or(out, segments[i].bitmap);
            }
            return out;
        }

        private int lowerBoundByMax(long lo) {
            int loI = 0;
            int hiI = segments.length - 1;
            int ans = segments.length;
            while (loI <= hiI) {
                int mid = (loI + hiI) >>> 1;
                if (segments[mid].maxValue >= lo) {
                    ans = mid;
                    hiI = mid - 1;
                } else {
                    loI = mid + 1;
                }
            }
            return ans;
        }

        /** 按值域把 distinct 值均分成至多 {@link #RANGE_SEGMENTS} 段，每段一个合并位图。 */
        private static Segment[] buildSegments(TreeMap<Long, long[]> postings) {
            int distinct = postings.size();
            if (distinct == 0) {
                return new Segment[0];
            }
            int segCount = Math.min(RANGE_SEGMENTS, distinct);
            var keys = new ArrayList<>(postings.keySet()); // TreeMap 键天然升序

            Segment[] segs = new Segment[segCount];
            int idx = 0;
            for (int i = 0; i < segCount; i++) {
                int from = (int) ((long) i * distinct / segCount);
                int to = (int) ((long) (i + 1) * distinct / segCount) - 1;
                if (to < from) {
                    continue; // distinct < segCount 时可能出现空段
                }
                long[] merged = null;
                for (int k = from; k <= to; k++) {
                    merged = (merged == null) ? postings.get(keys.get(k)) : or(merged, postings.get(keys.get(k)));
                }
                segs[idx++] = new Segment(keys.get(from), keys.get(to), merged);
            }
            if (idx == segCount) {
                return segs;
            }
            Segment[] trimmed = new Segment[idx];
            System.arraycopy(segs, 0, trimmed, 0, idx);
            return trimmed;
        }
    }

    /** 值域中的一个分段。 */
    static final class Segment {
        final long minValue;
        final long maxValue;
        final long[] bitmap;

        Segment(long minValue, long maxValue, long[] bitmap) {
            this.minValue = minValue;
            this.maxValue = maxValue;
            this.bitmap = bitmap;
        }
    }
}
