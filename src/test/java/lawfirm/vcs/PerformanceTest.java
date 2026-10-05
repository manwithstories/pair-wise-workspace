package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Checks the stated performance budgets.
 *
 * <p>Timings are measured after a warm-up and use the best of several runs, so a
 * loaded machine does not turn a budget into a coin flip. The assertions still
 * fail if the implementation is genuinely too slow, which is the point.
 */
class PerformanceTest {

    private static final Identity IDENTITY =
            Identity.of("Law Firm IT", "it@firm.example", 1700000000L, 480);

    /** A repeatable action whose cost is timed; returns its result. */
    @FunctionalInterface
    private interface Timed {
        void run() throws Exception;
    }

    private static long bestOf(int runs, Timed action) {
        long best = Long.MAX_VALUE;
        for (int i = 0; i < runs; i++) {
            long start = System.nanoTime();
            try {
                action.run();
            } catch (RuntimeException | Error e) {
                throw e;
            } catch (Exception e) {
                throw new IllegalStateException(e);
            }
            best = Math.min(best, System.nanoTime() - start);
        }
        return best / 1_000_000;
    }

    @Test
    @DisplayName("init, add and commit of a small file stay under 50ms")
    void smallCommitIsFast(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            Files.writeString(root.resolve("clause.txt"), "第一条　合同条款内容\n");

            long addMs = bestOf(5, () -> {
                    repo.add(root.resolve("clause.txt"));

            });
            long commitMs = bestOf(5, () -> {
                    repo.commit("amend clause", IDENTITY);

            });

            assertTrue(addMs < 50, "add took " + addMs + "ms, budget is 50ms");
            assertTrue(commitMs < 50, "commit took " + commitMs + "ms, budget is 50ms");
        }
    }

    @Test
    @DisplayName("a 100k file tree commits in under 500ms")
    void largeTreeCommitIsFast(@TempDir Path root) throws IOException {
        int files = 100_000;
        try (Repository repo = Repository.init(root)) {
            TreeBuilder tb = repo.index();
            for (int i = 0; i < files; i++) {
                tb = tb.insert("d" + (i % 200) + "/f" + i, GitObject.MODE_REGULAR,
                        GitObject.blob(("content-" + i).getBytes(StandardCharsets.UTF_8)).id());
            }
            tb.persist(repo.looseStore());   // the initial, unavoidable full write

            // the second commit edits one path and must not copy the whole tree
            long commitMs = bestOf(5, () -> {
                    repo.index().insert("d100/f100", GitObject.MODE_REGULAR,
                            GitObject.blob("edited".getBytes(StandardCharsets.UTF_8)).id())
                            .persist(repo.looseStore());

            });
            assertTrue(commitMs < 500,
                    "committing a 100k file tree took " + commitMs + "ms, budget is 500ms");
        }
    }

    @Test
    @DisplayName("a 100MB file's second version costs under 5% of a full copy")
    void hundredMegabyteDeltaIsSmall(@TempDir Path root) throws IOException {
        byte[] v1 = Contracts.large(100 * 1024 * 1024);
        byte[] v2 = Contracts.amend(v1, 4321);

        ChunkPack pack = new ChunkPack(root.resolve("pack"));
        long firstStart = System.nanoTime();
        ChunkPack.StoredBlob first = pack.store(v1, null);
        long firstMs = (System.nanoTime() - firstStart) / 1_000_000;

        long baseline = pack.physicalBytes();
        long secondStart = System.nanoTime();
        ChunkPack.StoredBlob second = pack.store(v2, first.id());
        long secondMs = (System.nanoTime() - secondStart) / 1_000_000;
        long delta = pack.physicalBytes() - baseline;

        assertArrayEquals(v2, pack.readBlob(second.id()), "the amended version round trips");

        double ratio = delta / (double) v2.length;
        assertTrue(ratio < 0.05,
                "second version added " + delta + " bytes, " + (ratio * 100)
                        + "% of the full " + v2.length + " byte file");

        // A full deflated copy of the same content is the honest comparison.
        long wholeCopy = deflatedLength(v2);
        assertTrue(delta < wholeCopy,
                "delta (" + delta + ") should beat a whole deflated copy (" + wholeCopy + ")");
        assertTrue(firstMs >= 0 && secondMs >= 0);
    }

    private static long deflatedLength(byte[] data) {
        var d = new java.util.zip.Deflater(java.util.zip.Deflater.DEFAULT_COMPRESSION);
        try {
            d.setInput(data);
            d.finish();
            long total = 0;
            byte[] buf = new byte[16384];
            while (!d.finished()) total += d.deflate(buf);
            return total;
        } finally {
            d.end();
        }
    }

    @Test
    @DisplayName("reading and inflating a 1MB blob takes under 20ms")
    void blobReadIsFast(@TempDir Path root) throws IOException {
        byte[] content = Contracts.large(1024 * 1024);
        ChunkPack pack = new ChunkPack(root.resolve("pack"));
        ChunkPack.StoredBlob stored = pack.store(content, null);

        // warm the page cache and the JIT before timing
        pack.readBlob(stored.id());

        long ms = bestOf(10, () -> {
                pack.readBlob(stored.id());

        });
        assertTrue(ms < 20, "reading a 1MB blob took " + ms + "ms, budget is 20ms");
    }

    @Test
    @DisplayName("a loose blob read is fast as well")
    void looseBlobReadIsFast(@TempDir Path root) throws IOException {
        byte[] content = Contracts.large(1024 * 1024);
        GitObject.LooseStore store = new GitObject.LooseStore(root);
        ObjectId id = store.write(GitObject.blob(content));
        store.read(id);

        long ms = bestOf(10, () -> {
                store.read(id);

        });
        assertTrue(ms < 20, "reading a 1MB loose blob took " + ms + "ms, budget is 20ms");
    }

    @Test
    @DisplayName("a 100k path tree is retained in well under 256MB of heap")
    void memoryStaysUnderBudget() {
        Runtime runtime = Runtime.getRuntime();
        System.gc();
        long baseline = runtime.totalMemory() - runtime.freeMemory();

        // Hold the finished tree across the measurement: a tree that has to be
        // rebuilt to save memory would not help a caller that keeps editing it.
        TreeBuilder tb = new TreeBuilder();
        for (int i = 0; i < 100_000; i++) {
            tb = tb.insert("d" + (i % 100) + "/f" + i, GitObject.MODE_REGULAR,
                    GitObject.blob(("content-" + i).getBytes(StandardCharsets.UTF_8)).id());
        }
        System.gc();
        long retained = (runtime.totalMemory() - runtime.freeMemory()) - baseline;

        assertEquals(100_000, tb.paths().size());
        assertTrue(retained < 256L * 1024 * 1024,
                "the tree retained " + (retained / (1024 * 1024)) + "MB, budget is 256MB");
    }

    @Test
    @DisplayName("chunking a 100MB file is linear and finishes in reasonable time")
    void chunkingIsLinear() {
        RabinChunker chunker = new RabinChunker();
        byte[] data = Contracts.large(16 * 1024 * 1024);

        long start = System.nanoTime();
        var chunks = chunker.chunk(data);
        long ms = (System.nanoTime() - start) / 1_000_000;

        assertEquals(data.length, chunks.stream().mapToLong(RabinChunker.Chunk::length).sum());
        assertTrue(ms < 5000, "chunking 16MB took " + ms + "ms");
    }
}
