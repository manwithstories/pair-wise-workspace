package com.carrier.sigstore;

import java.io.IOException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

/**
 * 测试公用脚手架：话单 schema、批量写入、临时目录。
 *
 * <p>schema 刻意照搬运营商画像查询的真实维度：地市 / 套餐 / 终端品牌 / 时间桶 / 话单量。
 */
final class TestSupport {

    static final String CITY = "city";
    static final String PLAN = "plan";
    static final String BRAND = "brand";
    static final String BUCKET = "bucket";
    static final String AMOUNT = "amount";
    static final String ROW_ID = "rowId";

    /** rowId 不参与过滤且基数极高，声明为 noIndex，用来验证高基数列会放弃位图。 */
    static Schema schema() {
        return Schema.builder()
                .intColumn(CITY)
                .intColumn(PLAN)
                .intColumn(BRAND)
                .longColumn(BUCKET)
                .longColumn(AMOUNT)
                .longColumn(ROW_ID)
                .noIndex(ROW_ID)
                .rowsPerPage(64)
                .build();
    }

    static ColumnStore open(Path dir, Schema schema, CrashInjector injector) throws IOException {
        WALJournal wal = new WALJournal(dir.resolve("wal.log"));
        return new ColumnStore(dir, schema, new SnapshotManager(), wal, injector);
    }

    /** 打开并执行恢复——这就是"重启进程"在测试里的等价物。 */
    static ColumnStore create(Path dir, Schema schema) throws IOException {
        return ColumnStore.open(dir, schema, new CrashInjector());
    }

    /** 一次事务批量写入 n 行。 */
    static void writeRows(ColumnStore store, int n, RowGenerator gen) {
        WriteTransaction tx = store.beginWrite();
        for (int i = 0; i < n; i++) {
            tx.append(gen.row(i));
        }
        tx.commit();
    }

    interface RowGenerator {
        Row row(int i);
    }

    /** 确定性的行生成：同样的 i 永远得到同样的行。 */
    static RowGenerator deterministic(int cities, int plans, int brands, int buckets) {
        return i -> Row.of(
                i % cities,                      // city
                (i / cities) % plans,            // plan
                (i * 7 + 3) % brands,            // brand
                (long) (i / 3) % buckets,        // bucket
                (i % 11) + 1,                    // amount
                (long) i);                       // rowId
    }

    static List<Row> scanAll(ColumnStore store, Filter.And f) {
        try (ColumnStore.ReadSnapshot snap = store.beginRead()) {
            return new QueryEngine(store).scan(f, List.of(), snap);
        }
    }

    static String rowsToString(List<Row> rows) {
        StringBuilder sb = new StringBuilder();
        for (Row r : rows) sb.append(r).append('\n');
        return sb.toString();
    }

    /** 临时目录，测试结束由 JUnit 清理。 */
    static Path tempDir(String name) throws IOException {
        Path p = java.nio.file.Files.createTempDirectory("sigstore-" + name + "-");
        p.toFile().deleteOnExit();
        return p;
    }

    static List<Long> bucketKeys(List<QueryEngine.Bucket> buckets) {
        List<Long> out = new ArrayList<>();
        for (QueryEngine.Bucket b : buckets) out.add(b.key());
        return out;
    }
}
