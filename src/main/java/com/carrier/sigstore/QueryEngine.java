package com.carrier.sigstore;

import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * 查询执行：位图下推 + 列向量回读 + 确定性聚合。
 *
 * <p><b>确定性是硬约束</b>。之所以能保证，靠的是三件事而非"小心排序"：
 * <ol>
 *   <li>结果行序恒为 {@code rowId 升序}。rowId 在提交时分配，因此天然等价于提交序，
 *       与插入顺序的任何抖动、{@code HashMap} 迭代序都无关。</li>
 *   <li>聚合桶一律用 {@link TreeMap}，桶序由业务键决定；比较器全是 {@code comparingLong}，
 *       不含任何按对象身份或哈希值做 tie-break 的分支。</li>
 *   <li>{@link #topN} 的比较器是<b>全序</b>：先比业务键，并列时按 rowId 升序兜底。
 *       所以"同一 SQL 两次执行 Top10 顺序不同"这种投诉在结构上不可能发生。</li>
 * </ol>
 *
 * <p>快照可见性口径：所有扫描统一取 {@code store.visibleRows(snapshot.commitSeq())}。
 * 这样即便位图里已经含有快照之后的行（位图按提交实时增长），扫描循环的
 * 上界也会把它们裁掉，不会泄露给旧快照的读者。
 */
public final class QueryEngine {

    private final ColumnStore store;

    public QueryEngine(ColumnStore store) {
        this.store = store;
    }

    /** 时间桶聚合结果；桶键有序，故输出序列确定。 */
    public record Bucket(long key, long count) {
    }

    /** TopN 结果：业务键 + rowId 构成全序，无并列歧义。 */
    public record TopEntry(String column, long value, long rowId) {
    }

    /**
     * 过滤扫描。命中集合先由位图求出，再按 rowId 升序回读列。
     *
     * <p>下推可用时，谓词涉及的列已经"判定过"，因此只回读<b>投影需要</b>的列；
     * 投影为空时甚至不必回读任何列数据——位图本身就是答案。
     */
    public List<Row> scan(Filter.And filter, List<String> projection, ColumnStore.ReadSnapshot snap) {
        long seq = snap.commitSeq();
        int rowCount = toRowCount(store.visibleRows(seq));
        if (rowCount == 0) return List.of();

        java.util.BitSet mask = pushdownMask(filter, rowCount);
        int[] cols = resolveProjection(projection);
        Schema schema = store.schema();
        int rpp = schema.rowsPerPage();

        // 下推未覆盖谓词（mask==null）时，必须逐行回退求值，否则会漏过滤。
        // 位图是"预筛"不是"判定"：任何没能下推的谓词都要在这里补上，
        // 否则按高基数列过滤会静默地返回全表。
        boolean needResidualCheck = mask == null && filter != null && !filter.isEmpty();

        List<Row> out = new ArrayList<>();
        for (int rowId = 0; rowId < rowCount; rowId++) {   // 天然升序，无需再排
            if (mask != null && !mask.get(rowId)) continue;
            int rg = rowId / rpp, slot = rowId % rpp;
            long[] vals = new long[cols.length];
            boolean ok = true;
            for (int i = 0; i < cols.length; i++) {
                ColumnStore.PageVersion v = store.readPage(rg, cols[i], seq);
                if (v == null || slot >= v.rowCount) { ok = false; break; }
                vals[i] = Page.readValue(v.view(), schema, cols[i], slot);
            }
            if (!ok) continue;
            if (needResidualCheck && !matches(filter, vals)) continue;
            out.add(Row.of(vals));
        }
        return out;
    }

    /** 回退路径用：读出该行的全部列取值；任一列不可见则返回 null。 */
    private long[] readAll(int rg, int slot, long seq, Schema schema) {
        long[] vals = new long[schema.columnCount()];
        for (int c = 0; c < schema.columnCount(); c++) {
            ColumnStore.PageVersion v = store.readPage(rg, c, seq);
            if (v == null || slot >= v.rowCount) return null;
            vals[c] = Page.readValue(v.view(), schema, c, slot);
        }
        return vals;
    }

    /** 逐谓词求值（回退路径）。vals 是全列取值，下标即列号。 */
    private static boolean matches(Filter.And f, long[] vals) {
        for (Filter p : f.filters()) {
            long v = vals[p.columnId()];
            boolean hit = switch (p.kind()) {
                case EQ -> v == p.values()[0];
                case IN -> {
                    boolean found = false;
                    for (long want : p.values()) {
                        if (want == v) { found = true; break; }
                    }
                    yield found;
                }
                case RANGE -> (p.loInclusive() ? v >= p.lo() : v > p.lo())
                        && (p.hiInclusive() ? v <= p.hi() : v < p.hi());
            };
            if (!hit) return false;
        }
        return true;
    }

    /** 全列扫描（投影为空时默认全部列）。 */
    public List<Row> scan(Filter.And filter, ColumnStore.ReadSnapshot snap) {
        return scan(filter, List.of(), snap);
    }

    private java.util.BitSet pushdownMask(Filter.And filter, int rowCount) {
        if (filter == null || filter.isEmpty()) return null;
        java.util.BitSet m = store.index().pushdown(filter);
        if (m == null) return null;
        // 位图可能比可见行数更长（后续提交已并入），裁到快照边界
        m.clear(rowCount, Math.max(rowCount, m.length()));
        return m;
    }

    private int[] resolveProjection(List<String> projection) {
        if (projection == null || projection.isEmpty()) {
            int[] all = new int[store.schema().columnCount()];
            for (int i = 0; i < all.length; i++) all[i] = i;
            return all;
        }
        int[] cols = new int[projection.size()];
        for (int i = 0; i < cols.length; i++) cols[i] = store.schema().columnId(projection.get(i));
        return cols;
    }

    private static int toRowCount(long rows) {
        return (int) Math.min(rows, Integer.MAX_VALUE);
    }

    /**
     * 按 key 列做时间桶计数。桶用 TreeMap，输出按桶键升序。
     */
    public List<Bucket> aggregateByBucket(Filter.And filter, String keyColumn, ColumnStore.ReadSnapshot snap) {
        Schema schema = store.schema();
        int keyId = schema.columnId(keyColumn);
        int rpp = schema.rowsPerPage();
        long seq = snap.commitSeq();
        int rowCount = toRowCount(store.visibleRows(seq));
        if (rowCount == 0) return List.of();

        java.util.BitSet mask = pushdownMask(filter, rowCount);
        boolean residual = mask == null && filter != null && !filter.isEmpty();
        TreeMap<Long, Long> buckets = new TreeMap<>();
        for (int rowId = 0; rowId < rowCount; rowId++) {
            if (mask != null && !mask.get(rowId)) continue;
            int rg = rowId / rpp, slot = rowId % rpp;
            ColumnStore.PageVersion v = store.readPage(rg, keyId, seq);
            if (v == null || slot >= v.rowCount) continue;
            if (residual) {
                // 回退路径：谓词列可能不在 key 列里，必须按全列取值判定
                long[] vals = readAll(rg, slot, seq, schema);
                if (vals == null || !matches(filter, vals)) continue;
            }
            long key = Page.readValue(v.view(), schema, keyId, slot);
            buckets.merge(key, 1L, Long::sum);      // TreeMap.merge：顺序确定
        }
        List<Bucket> out = new ArrayList<>(buckets.size());
        for (Map.Entry<Long, Long> e : buckets.entrySet()) out.add(new Bucket(e.getKey(), e.getValue()));
        return out;
    }

    /**
     * TopN。比较器是全序：业务键降序，并列按 rowId 升序。
     * 两次执行同一语句必然给出逐项相同的序列。
     */
    public List<TopEntry> topN(Filter.And filter, String valueColumn, int n, ColumnStore.ReadSnapshot snap) {
        Schema schema = store.schema();
        int colId = schema.columnId(valueColumn);
        int rpp = schema.rowsPerPage();
        long seq = snap.commitSeq();
        int rowCount = toRowCount(store.visibleRows(seq));
        if (rowCount == 0) return List.of();

        java.util.BitSet mask = pushdownMask(filter, rowCount);
        boolean residual = mask == null && filter != null && !filter.isEmpty();
        List<TopEntry> entries = new ArrayList<>();
        for (int rowId = 0; rowId < rowCount; rowId++) {
            if (mask != null && !mask.get(rowId)) continue;
            int rg = rowId / rpp, slot = rowId % rpp;
            ColumnStore.PageVersion v = store.readPage(rg, colId, seq);
            if (v == null || slot >= v.rowCount) continue;
            if (residual) {
                long[] vals = readAll(rg, slot, seq, schema);
                if (vals == null || !matches(filter, vals)) continue;
            }
            long val = Page.readValue(v.view(), schema, colId, slot);
            entries.add(new TopEntry(valueColumn, val, rowId));
        }
        Comparator<TopEntry> cmp = Comparator
                .comparingLong(TopEntry::value).reversed()
                .thenComparingLong(TopEntry::rowId);
        entries.sort(cmp);
        return entries.size() <= n ? entries : new ArrayList<>(entries.subList(0, n));
    }

    /** 多维组合计数（地市+套餐+终端品牌）。组合键按分量字典序比较，与哈希无关。 */
    public List<GroupCount> groupCount(Filter.And filter, List<String> columns, ColumnStore.ReadSnapshot snap) {
        Schema schema = store.schema();
        int[] ids = new int[columns.size()];
        for (int i = 0; i < columns.size(); i++) ids[i] = schema.columnId(columns.get(i));
        int rpp = schema.rowsPerPage();
        long seq = snap.commitSeq();
        int rowCount = toRowCount(store.visibleRows(seq));
        if (rowCount == 0) return List.of();

        java.util.BitSet mask = pushdownMask(filter, rowCount);
        boolean residual = mask == null && filter != null && !filter.isEmpty();
        TreeMap<GroupKey, Long> groups = new TreeMap<>();
        for (int rowId = 0; rowId < rowCount; rowId++) {
            if (mask != null && !mask.get(rowId)) continue;
            int rg = rowId / rpp, slot = rowId % rpp;
            if (residual) {
                long[] all = readAll(rg, slot, seq, schema);
                if (all == null || !matches(filter, all)) continue;
            }
            long[] key = new long[ids.length];
            boolean ok = true;
            for (int i = 0; i < ids.length; i++) {
                ColumnStore.PageVersion v = store.readPage(rg, ids[i], seq);
                if (v == null || slot >= v.rowCount) { ok = false; break; }
                key[i] = Page.readValue(v.view(), schema, ids[i], slot);
            }
            if (ok) groups.merge(new GroupKey(key), 1L, Long::sum);
        }
        List<GroupCount> out = new ArrayList<>(groups.size());
        for (Map.Entry<GroupKey, Long> e : groups.entrySet()) {
            out.add(new GroupCount(List.copyOf(columns), e.getKey().values, e.getValue()));
        }
        return out;
    }

    /** 一行分组结果：各分组列取值 + 计数。 */
    public record GroupCount(List<String> columns, long[] values, long count) {
        @Override
        public String toString() {
            StringBuilder sb = new StringBuilder("(");
            for (int i = 0; i < values.length; i++) {
                if (i > 0) sb.append(", ");
                sb.append(columns.get(i)).append('=').append(values[i]);
            }
            return sb.append(") -> ").append(count).toString();
        }
    }

    /** 分组键：按分量字典序比较。 */
    static final class GroupKey implements Comparable<GroupKey> {
        final long[] values;

        GroupKey(long[] values) {
            this.values = values;
        }

        @Override
        public int compareTo(GroupKey o) {
            int n = Math.min(values.length, o.values.length);
            for (int i = 0; i < n; i++) {
                int c = Long.compare(values[i], o.values[i]);
                if (c != 0) return c;
            }
            return Integer.compare(values.length, o.values.length);
        }

        @Override
        public boolean equals(Object o) {
            return o instanceof GroupKey g && Arrays.equals(values, g.values);
        }

        @Override
        public int hashCode() {
            return Arrays.hashCode(values);
        }
    }
}
