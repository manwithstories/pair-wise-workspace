package com.carrier.sigstore;

import java.io.IOException;
import java.nio.file.Path;

/** 批量装载工具：大批量压测时避免逐行写页带来的 O(rows x pagesize) 拷贝开销。 */
final class TestStore2 {

    private TestStore2() {
    }

    /**
     * 用批量装载路径写入 n 行：整批只做一次 fsync。
     * 压测需要的是"数据已经就绪"，不是"逐条接入生产流"。
     */
    static ColumnStore createFast(Path dir, Schema schema, int n) throws IOException {
        ColumnStore store = ColumnStore.open(dir, schema, new CrashInjector());
        BulkLoad load = store.beginBulkLoad();
        for (int i = 0; i < n; i++) {
            load.append(rowFor(schema, i));
        }
        load.finish();
        return store;
    }

    private static Row rowFor(Schema schema, int i) {
        return Row.of(
                i % 20,
                (i / 20) % 10,
                (i * 7 + 3) % 30,
                (long) (i / 3) % 100,
                (i % 11) + 1,
                (long) i);
    }
}
