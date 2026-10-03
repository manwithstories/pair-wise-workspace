package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Random;
import java.util.concurrent.ForkJoinPool;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The parallel executor must return exactly what a single-threaded scan returns: same Top-K set,
 * same order, whatever order the segments finish in.
 */
class ParallelQueryExecutorTest {

    @TempDir
    Path tmp;

    /** Builds n segments of KD trees plus their metadata. */
    private record Fixture(List<SegmentLoader.SegmentMeta> metas, List<KDTreeIndex> trees,
                           List<GeoFeature> all) {
    }

    private Fixture buildSegments(int segments, int perSegment, long seed) {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 64L * 1024 * 1024);
        List<GeoFeature> all = new ArrayList<>();
        List<SegmentLoader.SegmentMeta> metas = new ArrayList<>();
        List<KDTreeIndex> trees = new ArrayList<>();
        long id = 0;
        for (int s = 0; s < segments; s++) {
            List<GeoFeature> chunk = new ArrayList<>();
            Random r = new Random(seed + s);
            for (int i = 0; i < perSegment; i++) {
                chunk.add(GeoFeature.point(id++, FeatureType.POI, r.nextDouble() * 1000,
                        r.nextDouble() * 1000));
            }
            Path dir = tmp.resolve("seg-" + s);
            SegmentStore store = SegmentStore.create(dir.resolve(SegmentLoader.POINT_FILE),
                    perSegment + SegmentLoader.EXTRA_ROWS_PER_SEGMENT, 0);
            for (GeoFeature f : chunk) {
                store.append(f);
            }
            store.flushHeader();
            metas.add(new SegmentLoader.SegmentMeta(s, dir, perSegment, 0,
                    new BBox(0, 0, 1000, 1000), 0));
            trees.add(new KDTreeIndex(store));
            all.addAll(chunk);
        }
        return new Fixture(metas, trees, all);
    }

    @Test
    @DisplayName("parallel Top-K equals the single-threaded answer for many probes")
    void parallelMatchesSequential() {
        Fixture f = buildSegments(8, 400, 3);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(4)) {
            Random r = new Random(101);
            for (int t = 0; t < 200; t++) {
                GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
                int k = 1 + r.nextInt(50);
                assertEquals(TestData.bruteForceKnn(f.all(), probe, k, null),
                        exec.knn(f.metas(), f.trees(), probe, k),
                        "parallel result must equal an exhaustive scan (probe=" + probe + ")");
            }
        }
    }

    @Test
    @DisplayName("the executor really dispatches one task per segment")
    void dispatchesPerSegment() {
        Fixture f = buildSegments(6, 100, 5);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(4)) {
            exec.knn(f.metas(), f.trees(), new GeoPoint(500, 500), 10);
            assertEquals(6, exec.dispatchedTasks());
        }
    }

    @Test
    @DisplayName("mergeTopK is order-independent")
    void mergeIsOrderIndependent() {
        List<Neighbor> pool = new ArrayList<>();
        Random r = new Random(17);
        for (int i = 0; i < 200; i++) {
            pool.add(new Neighbor(GeoFeature.point(i, FeatureType.POI, r.nextDouble(),
                    r.nextDouble()), r.nextDouble()));
        }
        Collections.shuffle(pool, r);
        int k = 20;
        List<Neighbor> expected = new ArrayList<>(pool);
        expected.sort(Neighbor::compareTo);
        List<Neighbor> want = expected.subList(0, k);

        for (int trial = 0; trial < 20; trial++) {
            Collections.shuffle(pool, r);
            List<List<Neighbor>> chunks = new ArrayList<>();
            int i = 0;
            while (i < pool.size()) {
                int size = 1 + r.nextInt(10);
                chunks.add(new ArrayList<>(pool.subList(i, Math.min(pool.size(), i + size))));
                i += size;
            }
            assertEquals(want, ParallelQueryExecutor.mergeTopK(chunks, k));
        }
    }

    @Test
    @DisplayName("ties in distance are resolved by id, keeping results deterministic")
    void tiesAreDeterministic() {
        // Ten features all one unit from the probe.
        List<GeoFeature> data = new ArrayList<>();
        for (int i = 0; i < 10; i++) {
            data.add(GeoFeature.point(i, FeatureType.POI, 1, 0));
        }
        Fixture single = new Fixture(List.of(), List.of(), data);
        KDTreeIndex tree;
        try (SegmentStore store = SegmentStore.create(tmp.resolve("t.seg"), 10, 0)) {
            for (GeoFeature f : data) {
                store.append(f);
            }
            store.flushHeader();
            tree = new KDTreeIndex(store);
        }
        SegmentLoader.SegmentMeta meta = new SegmentLoader.SegmentMeta(0, tmp, 10, 0,
                new BBox(0, 0, 2, 2), 0);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(2)) {
            List<Neighbor> got = exec.knn(List.of(meta), List.of(tree), new GeoPoint(0, 0), 5);
            assertEquals(List.of(0L, 1L, 2L, 3L, 4L), got.stream().map(Neighbor::id).toList());
        }
    }

    @Test
    @DisplayName("range queries skip segments whose bounds miss the window")
    void rangeSearchRespectsSegmentBounds() {
        // Segment 0 holds parcels near the origin, segment 1 holds parcels far away.
        List<GeoFeature> near = new ArrayList<>();
        List<GeoFeature> far = new ArrayList<>();
        for (int i = 0; i < 20; i++) {
            near.add(GeoFeature.polygon(i, FeatureType.PARCEL,
                    new double[]{i, 0, i + 0.5, 0, i + 0.5, 1, i, 1}));
            far.add(GeoFeature.polygon(1000 + i, FeatureType.PARCEL,
                    new double[]{5000 + i, 5000, 5000 + i + 0.5, 5000, 5000 + i + 0.5, 5001, 5000 + i, 5001}));
        }
        List<SegmentLoader.SegmentMeta> metas = new ArrayList<>();
        List<RTreeIndex> trees = new ArrayList<>();
        int s = 0;
        for (List<GeoFeature> chunk : List.of(near, far)) {
            Path dir = tmp.resolve("seg" + s);
            int pool = 0;
            for (GeoFeature f : chunk) {
                pool += SegmentStore.vertexSlots(f);
            }
            SegmentStore store = SegmentStore.create(dir.resolve(SegmentLoader.AREA_FILE),
                    chunk.size() + 8, pool + 32);
            for (GeoFeature f : chunk) {
                store.append(f);
            }
            store.flushHeader();
            BBox b = chunk.get(0).isArea() ? BBox.envelope(chunk.get(0).vertices())
                    : BBox.ofPoint(0, 0);
            metas.add(new SegmentLoader.SegmentMeta(s, dir, 0, chunk.size(),
                    new BBox(0, 0, 1, 1), 0));
            trees.add(new RTreeIndex(store));
            s++;
        }
        // Give the far segment bounds that exclude the window.
        metas.set(1, new SegmentLoader.SegmentMeta(1, metas.get(1).directory(), 0, far.size(),
                new BBox(5000, 5000, 5100, 5100), 0));
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(4)) {
            List<GeoFeature> got = exec.rangeSearch(metas, trees, new BBox(-10, -10, 100, 100));
            assertEquals(20, got.size());
            assertTrue(got.stream().allMatch(f -> f.id() < 1000), "far segment leaked into results");
        }
    }

    @Test
    @DisplayName("a very wide window degrades to a parallel scan with the same answer")
    void wideWindowDegrades() {
        List<GeoFeature> data = TestData.parcels(600, 23, 1000);
        Path dir = tmp.resolve("all");
        int pool = 0;
        for (GeoFeature f : data) {
            pool += SegmentStore.vertexSlots(f);
        }
        SegmentStore store = SegmentStore.create(dir.resolve(SegmentLoader.AREA_FILE), 700, pool + 32);
        for (GeoFeature f : data) {
            store.append(f);
        }
        store.flushHeader();
        RTreeIndex rt = new RTreeIndex(store);
        List<SegmentLoader.SegmentMeta> metas = List.of(new SegmentLoader.SegmentMeta(0, dir, 0,
                data.size(), new BBox(0, 0, 1000, 1000), 0));
        List<RTreeIndex> trees = List.of(rt);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(4)) {
            BBox whole = new BBox(-100, -100, 2000, 2000);
            List<Long> viaTree = TestData.ids(exec.rangeSearch(metas, trees, whole));
            List<Long> viaScan = TestData.ids(exec.parallelScan(metas, trees, whole));
            assertEquals(TestData.bruteForceRange(data, whole), viaTree);
            assertEquals(viaTree, viaScan, "the degraded scan must agree with the tree");
        }
    }

    @Test
    @DisplayName("k larger than the corpus returns everything")
    void kExceedsCorpus() {
        Fixture f = buildSegments(3, 10, 29);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(2)) {
            assertEquals(30, exec.knn(f.metas(), f.trees(), new GeoPoint(1, 1), 1000).size());
        }
    }

    @Test
    @DisplayName("an external pool can be supplied and reused")
    void acceptsExternalPool() {
        Fixture f = buildSegments(2, 50, 31);
        ForkJoinPool pool = new ForkJoinPool(3);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(pool)) {
            assertEquals(20, exec.knn(f.metas(), f.trees(), new GeoPoint(5, 5), 20).size());
        } finally {
            pool.shutdown();
        }
    }

    @Test
    @DisplayName("concurrent queries agree with the sequential answer")
    void concurrentQueriesAreStable() {
        Fixture f = buildSegments(6, 300, 37);
        try (ParallelQueryExecutor exec = new ParallelQueryExecutor(4)) {
            List<List<Neighbor>> expected = new ArrayList<>();
            for (int i = 0; i < 20; i++) {
                GeoPoint p = new GeoPoint(i * 47 % 900 + 5, i * 91 % 900 + 5);
                expected.add(TestData.bruteForceKnn(f.all(), p, 25, null));
            }
            // Hammer the index from several threads; every answer must still be the same.
            List<Thread> threads = new ArrayList<>();
            for (int t = 0; t < 4; t++) {
                Thread th = new Thread(() -> {
                    for (int i = 0; i < 20; i++) {
                        GeoPoint p = new GeoPoint(i * 47 % 900 + 5, i * 91 % 900 + 5);
                        assertEquals(expected.get(i), exec.knn(f.metas(), f.trees(), p, 25));
                    }
                });
                threads.add(th);
                th.start();
            }
            for (Thread th : threads) {
                th.join();
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new AssertionError(e);
        }
    }
}
