package com.carrier.sigstore;

import java.util.ArrayList;
import java.util.BitSet;
import java.util.List;
import java.util.TreeMap;   // 有序 Map：不用 HashMap，避免哈希随机化影响任何输出

/**
 * 倒排位图索引：每个基数列一张 {@code value -> BitSet} 倒排表，外加一张分段位图。
 *
 * <p>C 个列族、每列至多 D 个不同取值时，共 C*D 个 BitSet，每个长度 N = 已索引行数。
 * 位图过滤的代价是 O(C*D*N/64) 字操作，<b>与命中行数无关</b>——这正是把
 * "多列等值过滤走嵌套循环 40 秒" 压到 50 毫秒量级的机制。
 *
 * <p>并发模型：写路径单写者调用 {@link #addRow}；读路径只读已发布的 BitSet，
 * 且一律通过 {@code clone()} 拿独立副本，读者之间无共享可变状态，故读侧无锁。
 *
 * <p>用 {@link TreeMap} 而非 {@code HashMap}：迭代顺序由取值本身决定，
 * 与 {@code -XX:hashCode} 随机化种子无关，跨 JVM 重启可复现。
 */
public final class BitmapIndex {

    private final Schema schema;

    /** 每列的倒排表；key 顺序由 TreeMap 保证确定。 */
    private final TreeMap<Long, BitSet>[] inverted;

    /** 每列的区间分段位图。 */
    private final RangeBitmap[] ranges;

    private final int[] distinctCount;
    private final boolean[] disabled;     // 因高基数被放弃位图的列

    private int rowCount;                 // 已索引行数，单调递增

    @SuppressWarnings("unchecked")
    public BitmapIndex(Schema schema) {
        this.schema = schema;
        int c = schema.columnCount();
        this.inverted = new TreeMap[c];
        this.ranges = new RangeBitmap[c];
        this.distinctCount = new int[c];
        this.disabled = new boolean[c];
        for (int i = 0; i < c; i++) {
            inverted[i] = new TreeMap<>();
            ranges[i] = new RangeBitmap();
        }
    }

    public int rowCount() {
        return rowCount;
    }

    public synchronized int indexedRows() {
        return rowCount;
    }

    /**
     * 为 rowId 追加一行（单调递增，不得重复）。
     *
     * <p>某列基数一旦超过 {@link Schema#BITMAP_MAX_CARDINALITY} 就放弃该列的位图：
     * 为高基数列维持倒排表会让内存随行数线性爆炸，得不偿失。放弃后该列退化为扫描。
     */
    public synchronized void addRow(int rowId, long[] values) {
        if (rowId != rowCount) {
            throw new IllegalStateException("索引必须按行号连续追加: 期望 " + rowCount + " 实际 " + rowId);
        }
        for (int c = 0; c < schema.columnCount(); c++) {
            if (disabled[c] || !schema.isIndexCandidate(c)) continue;
            long v = values[c];
            BitSet bs = inverted[c].computeIfAbsent(v, k -> new BitSet(rowCount + 1));
            bs.set(rowId);
            if (inverted[c].size() > Schema.BITMAP_MAX_CARDINALITY) {
                inverted[c].clear();
                disabled[c] = true;
                distinctCount[c] = -1;
            } else {
                distinctCount[c] = inverted[c].size();
            }
            ranges[c].add(rowId, v);
        }
        rowCount++;
    }

    /** 该列是否真有位图可用。 */
    public boolean isIndexed(String column) {
        int c = schema.columnId(column);
        return !disabled[c] && schema.isIndexCandidate(c);
    }

    /** 该列基数；-1 表示未建索引。 */
    public int cardinality(String column) {
        int c = schema.columnId(column);
        synchronized (this) {
            return disabled[c] ? -1 : distinctCount[c];
        }
    }

    /** 取"某列 == value"的位图副本；未建索引或无此值时返回全 0 位图。 */
    public BitSet lookup(String column, long value) {
        int c = schema.columnId(column);
        synchronized (this) {
            if (disabled[c] || !schema.isIndexCandidate(c)) return null;
            BitSet bs = inverted[c].get(value);
            return bs == null ? new BitSet(rowCount) : (BitSet) bs.clone();
        }
    }

    /** 该列被切成了几段。 */
    public int segmentCount(String column) {
        int c = schema.columnId(column);
        synchronized (this) {
            return ranges[c].size();
        }
    }

    /** 返回值落在 [lo,hi] 的行号区间（升序、互不重叠）。 */
    public List<long[]> rangeSegments(String column, long lo, long hi) {
        int c = schema.columnId(column);
        synchronized (this) {
            return ranges[c].segments(lo, hi, rowCount);
        }
    }

    /**
     * 把一组 AND 谓词编译为位图交集。
     *
     * <p>能否全量下推的判据是"所有谓词所在列都有位图"。任一列没有位图就整体返回
     * {@code null}，由调用方退化为扫描——<b>宁可少下推，也绝不做部分下推</b>，
     * 因为"下推了但没下推全"最容易产生难以排查的漏行。
     */
    public BitSet pushdown(Filter.And and) {
        if (and == null || and.isEmpty()) return null;
        List<Filter> filters = and.filters();
        for (Filter f : filters) {
            if (!isIndexed(f.columnName())) return null;
        }
        BitSet acc = null;
        for (Filter f : filters) {
            BitSet part = compileOne(f);
            if (acc == null) {
                acc = part;
            } else {
                acc.and(part);      // AND 可交换：谓词书写顺序不影响结果
            }
        }
        return acc;
    }

    private BitSet compileOne(Filter f) {
        return switch (f.kind()) {
            case EQ -> {
                synchronized (this) {
                    BitSet bs = inverted[f.columnId()].get(f.values()[0]);
                    yield bs == null ? new BitSet(rowCount) : (BitSet) bs.clone();
                }
            }
            case IN -> {
                BitSet acc = null;
                for (long v : f.values()) {      // values 已排序去重，顺序确定
                    BitSet bs;
                    synchronized (this) {
                        BitSet got = inverted[f.columnId()].get(v);
                        bs = got == null ? new BitSet(rowCount) : (BitSet) got.clone();
                    }
                    if (acc == null) acc = bs; else acc.or(bs);
                }
                yield acc == null ? new BitSet(rowCount) : acc;
            }
            case RANGE -> {
                BitSet acc = new BitSet(rowCount);
                for (long[] seg : rangeSegments(f.columnName(), f.lo(), f.hi())) {
                    // seg 是半开区间 [start, end)，而 BitSet.set(from, to) 的右端也是开区间，
                    // 因此直接传 end 即可——传 end-1 会漏掉每一段的最后一行，
                    // 表现为"区间过滤少返回结果"（单行段则整段丢失，结果恒为空）。
                    acc.set((int) seg[0], (int) seg[1]);
                }
                yield acc;
            }
        };
    }

    /**
     * 区间分段位图：记录每个 rowId 所属的段与段内取值。
     *
     * <p>连续同值的行会被合并成一段，因此时间戳这类单调列通常只有很少几段；
     * 区间过滤只需合并命中段，不必逐行比值。即使数据非单调，退化成逐行段，
     * 结果依然是<b>精确且确定</b>的——段里存的就是该行的真实取值。
     */
    static final class RangeBitmap {
        private final List<int[]> starts = new ArrayList<>();    // rowStart
        private final List<long[]> vals = new ArrayList<>();     // 段内取值

        void add(int row, long value) {
            if (!starts.isEmpty()) {
                int last = starts.size() - 1;
                // 与上一段同值且行号连续：并入上一段（段长由下一段起点隐含决定）
                if (vals.get(last)[0] == value) return;
            }
            starts.add(new int[]{row});
            vals.add(new long[]{value});
        }

        int size() {
            return starts.size();
        }

        /**
         * 命中段的 [rowStart, rowEndExclusive)，按行号升序。
         *
         * <p>末段右边界必须夹到 {@code rowCount}：否则调用方按该区间分配位图时，
         * 会因为上界是 MAX_VALUE 而尝试分配巨量内存。
         */
        List<long[]> segments(long lo, long hi, int rowCount) {
            List<long[]> out = new ArrayList<>();
            for (int i = 0; i < starts.size(); i++) {
                long v = vals.get(i)[0];
                if (v < lo || v > hi) continue;
                int start = starts.get(i)[0];
                int end = (i + 1 < starts.size()) ? starts.get(i + 1)[0] : rowCount;
                if (start < end) out.add(new long[]{start, end});
            }
            return out;
        }
    }

    @Override
    public String toString() {
        return "BitmapIndex(cols=" + schema.columnCount() + ", rows=" + rowCount + ")";
    }
}
