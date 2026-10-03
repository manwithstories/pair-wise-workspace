package com.carrier.sigstore;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;

/**
 * 列族（column family）定义。不可变，创建后线程安全。
 *
 * <p>行组内采用列主序（column-major）物理布局：每个列族在页负载里占一段定长块，
 * 块大小 = rowsPerPage * width。列向扫描因此是顺序访存；等值过滤下推到位图后，
 * 只需回读命中列对应的块，等值过滤的代价与列数无关。
 */
public final class Schema {

    public enum Type {
        INT(4), LONG(8);

        public final int width;

        Type(int width) {
            this.width = width;
        }
    }

    /** 单个列族的元数据。indexCandidate=false 表示基数不可控、不建位图。 */
    public record Column(String name, Type type, boolean indexCandidate) {
        static Column of(String name, Type type, boolean indexCandidate) {
            return new Column(Objects.requireNonNull(name, "列名"), Objects.requireNonNull(type, "列类型"), indexCandidate);
        }
    }

    /** 位图索引的适用上限：基数超过该阈值的列走扫描而不是位图。 */
    public static final int BITMAP_MAX_CARDINALITY = 10_000;

    private final List<Column> columns;
    private final Map<String, Integer> byName;   // LinkedHashMap：保持声明顺序
    private final int[] offsets;                 // 列块相对页负载起点的字节偏移
    private final int rowsPerPage;
    private final int rowWidth;
    private final int payloadBytes;

    private Schema(List<Column> columns, int rowsPerPage) {
        if (columns.isEmpty()) throw new IllegalArgumentException("至少需要一个列族");
        this.columns = List.copyOf(columns);

        int width = 0;
        for (Column c : columns) width += c.type().width;
        if (rowsPerPage <= 0) {
            // 默认按 64KB 页容量推导行数，保证列块总能塞进一页。
            rowsPerPage = Math.max(1, Page.MAX_PAYLOAD_DEFAULT / width);
        }
        if (width <= 0) throw new IllegalArgumentException("行宽必须为正");
        if (rowsPerPage * width > Page.MAX_PAYLOAD_DEFAULT) {
            throw new IllegalArgumentException("行宽 " + width + " x 行数 " + rowsPerPage + " 超出单页容量");
        }

        LinkedHashMap<String, Integer> m = new LinkedHashMap<>();
        int[] offs = new int[columns.size()];
        int off = 0;
        for (int i = 0; i < columns.size(); i++) {
            Column c = columns.get(i);
            if (m.put(c.name(), i) != null) throw new IllegalArgumentException("重复列名: " + c.name());
            offs[i] = off;
            off += rowsPerPage * c.type().width;
        }
        this.byName = m;
        this.offsets = offs;
        this.rowsPerPage = rowsPerPage;
        this.rowWidth = width;
        this.payloadBytes = off;
    }

    public static Builder builder() {
        return new Builder();
    }

    public int columnCount() {
        return columns.size();
    }

    public Column column(int i) {
        return columns.get(i);
    }

    public List<Column> columns() {
        return columns;
    }

    public boolean hasColumn(String name) {
        return byName.containsKey(name);
    }

    public int columnId(String name) {
        Integer id = byName.get(name);
        if (id == null) throw new IllegalArgumentException("未知列: " + name);
        return id;
    }

    public int offsetOf(int columnId) {
        return offsets[columnId];
    }

    /** 一个行组能容纳的行数；一个行组恰好对应每个列族各一个页。 */
    public int rowsPerPage() {
        return rowsPerPage;
    }

    public int rowWidth() {
        return rowWidth;
    }

    public int payloadBytes() {
        return payloadBytes;
    }

    public boolean isIndexCandidate(int columnId) {
        return columns.get(columnId).indexCandidate();
    }

    @Override
    public String toString() {
        return "Schema" + columns;
    }

    public static final class Builder {
        private final List<Column> cols = new ArrayList<>();
        private int rowsPerPage = -1;

        public Builder intColumn(String name) {
            cols.add(Column.of(name, Type.INT, true));
            return this;
        }

        public Builder longColumn(String name) {
            cols.add(Column.of(name, Type.LONG, true));
            return this;
        }

        /** 标记为不建位图（高基数列，如时间戳、流水号）。 */
        public Builder noIndex(String name) {
            for (int i = 0; i < cols.size(); i++) {
                if (cols.get(i).name().equals(name)) {
                    cols.set(i, Column.of(name, cols.get(i).type(), false));
                    return this;
                }
            }
            throw new IllegalArgumentException("未知列: " + name);
        }

        public Builder rowsPerPage(int n) {
            if (n <= 0) throw new IllegalArgumentException("rowsPerPage 必须为正");
            this.rowsPerPage = n;
            return this;
        }

        public Schema build() {
            return new Schema(cols, rowsPerPage);
        }
    }
}
