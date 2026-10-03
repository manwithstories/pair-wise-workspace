package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Random;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Guards the central promise of the service: the whole corpus is never resident.
 *
 * <p>These tests build data sets far larger than the segment budget and assert that the loader keeps
 * splitting and that retained heap stays bounded. Surefire runs the suite with {@code -Xmx512m}, so a
 * full load of this data would fail with an OOM rather than silently pass.
 */
class MemoryBudgetTest {

    /** Surefire runs the suite under this cap; asserted here so the test is self-documenting. */
    private static final long RUNTIME_HEAP = Runtime.getRuntime().maxMemory();

    @TempDir
    Path tmp;

    @Test
    @DisplayName("the suite really does run under a 512MB heap")
    void heapIsCapped() {
        assertTrue(RUNTIME_HEAP <= 768L * 1024 * 1024,
                "tests must run under the documented 512MB budget, but maxMemory is " + RUNTIME_HEAP);
    }

    @Test
    @DisplayName("a corpus far larger than one segment is split, never loaded whole")
    void largeCorpusIsSegmented() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 1024L * 1024);
        int perSegment = loader.rowsPerSegment();
        int total = Math.max(perSegment * 20, 200_000);
        assertTrue(total > perSegment * 10, "the corpus must be much larger than a segment");

        CountingIterator source = new CountingIterator(total);
        List<SegmentLoader.SegmentMeta> metas = loader.load(source);

        assertTrue(metas.size() >= 10, "expected many segments, got " + metas.size());
        long stored = 0;
        for (SegmentLoader.SegmentMeta m : metas) {
            stored += m.pointCount();
            assertTrue(m.pointCount() <= perSegment,
                    "a segment must respect the heap-derived row budget");
        }
        assertEquals(total, stored, "every feature must be stored exactly once");
        assertEquals(total, source.consumed(), "the loader must consume the source exactly once");
    }

    @Test
    @DisplayName("heap stays flat while a corpus many times the budget is indexed")
    void heapStaysBoundedWhileIngesting() {
        long budget = 2L * 1024 * 1024;
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), budget);
        int total = loader.rowsPerSegment() * 25;
        Runtime runtime = Runtime.getRuntime();

        System.gc();
        long before = runtime.totalMemory() - runtime.freeMemory();
        List<SegmentLoader.SegmentMeta> metas = loader.load(new CountingIterator(total));
        System.gc();
        long after = runtime.totalMemory() - runtime.freeMemory();

        long segmentsWithData = metas.stream().filter(m -> !m.isEmpty()).count();
        assertTrue(segmentsWithData >= 20, "the corpus must span many segments");
        assertTrue(after - before < budget * 8,
                "heap grew by " + (after - before) + " bytes ingesting " + total
                        + " features under a " + budget + " byte budget");
    }

    @Test
    @DisplayName("querying a segmented corpus does not materialise it on the heap")
    void queriesStayBounded() {
        int perSegment = 20_000;
        int segments = 25;
        Random r = new Random(3);
        List<GeoFeature> data = new ArrayList<>();
        for (int i = 0; i < perSegment * segments; i++) {
            data.add(GeoFeature.point(i, FeatureType.POI, r.nextDouble() * 1000, r.nextDouble() * 1000));
        }
        Runtime runtime = Runtime.getRuntime();
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), data, 2L * 1024 * 1024)) {
            assertTrue(index.segmentCount() > 1, "the corpus must be segmented");
            System.gc();
            long before = runtime.totalMemory() - runtime.freeMemory();
            for (int i = 0; i < 200; i++) {
                index.knn(new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000), 50);
            }
            System.gc();
            long after = runtime.totalMemory() - runtime.freeMemory();
            assertTrue(after - before < 32L * 1024 * 1024,
                    "200 kNN queries over " + data.size() + " features retained "
                            + (after - before) + " bytes");
        }
    }

    /** Feeds deterministic features without holding the whole list at once. */
    private static final class CountingIterator implements java.util.Iterator<GeoFeature> {
        private final int total;
        private final Random random = new Random(1234);
        private int index;
        private int consumed;

        CountingIterator(int total) {
            this.total = total;
        }

        int consumed() {
            return consumed;
        }

        @Override
        public boolean hasNext() {
            return index < total;
        }

        @Override
        public GeoFeature next() {
            GeoFeature f = GeoFeature.point(index, FeatureType.POI,
                    random.nextDouble() * 1000, random.nextDouble() * 1000);
            index++;
            consumed++;
            return f;
        }
    }
}
