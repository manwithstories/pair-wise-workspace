package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** Segment planning, bounded-memory loading and the external merge. */
class SegmentLoaderTest {

    @TempDir
    Path tmp;

    private static List<GeoFeature> mixed(int points, int parcels, long seed) {
        return TestData.merge(TestData.points(points, seed, 1000),
                TestData.parcels(parcels, seed + 1, 1000));
    }

    @Test
    @DisplayName("a small data set fits in a single segment")
    void singleSegment() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 64 * 1024 * 1024);
        List<SegmentLoader.SegmentMeta> metas = loader.load(mixed(500, 200, 1));
        assertEquals(1, metas.size());
        assertEquals(700, metas.get(0).totalCount());
        assertEquals(500, metas.get(0).pointCount());
        assertEquals(200, metas.get(0).areaCount());
    }

    @Test
    @DisplayName("a tiny heap budget forces many segments, never a full load")
    void budgetSplitsIntoManySegments() {
        // A budget that allows only a few rows per segment.
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 64 * 1024, 0.25);
        long perSegmentBudget = (long) (64 * 1024 * 0.25);
        assertTrue(loader.rowsPerSegment() <= perSegmentBudget / SegmentStore.rowBytes() + 1,
                "the planned row budget must fit inside the per-segment heap share");
        List<SegmentLoader.SegmentMeta> metas = loader.load(mixed(2000, 1000, 2));
        assertTrue(metas.size() > 1, "a small budget must split the data");
        int totalPoints = 0;
        int totalAreas = 0;
        for (SegmentLoader.SegmentMeta m : metas) {
            totalPoints += m.pointCount();
            totalAreas += m.areaCount();
            // Each row region is sized from the budget, so neither store may exceed it.
            assertTrue(m.pointCount() <= loader.rowsPerSegment(),
                    "point region exceeded the planned row budget");
            assertTrue(m.areaCount() <= loader.rowsPerSegment(),
                    "area region exceeded the planned row budget");
        }
        assertEquals(2000, totalPoints, "every point must land in exactly one segment");
        assertEquals(1000, totalAreas, "every parcel must land in exactly one segment");
    }

    @Test
    @DisplayName("no feature is lost or duplicated across segment boundaries")
    void everyFeatureLandsExactlyOnce() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 256 * 1024);
        List<GeoFeature> data = mixed(3000, 1500, 3);
        List<SegmentLoader.SegmentMeta> metas = loader.load(data);
        // Points and parcels have separate id spaces here, so track them independently.
        Set<Long> seenPoints = new HashSet<>();
        Set<Long> seenParcels = new HashSet<>();
        int actual = 0;
        for (SegmentLoader.SegmentMeta meta : metas) {
            if (meta.pointCount() > 0) {
                SegmentStore store = SegmentStore.openReadOnly(
                        meta.directory().resolve(SegmentLoader.POINT_FILE));
                for (int i = 0; i < store.count(); i++) {
                    assertTrue(seenPoints.add(store.idAt(i)),
                            "point " + store.idAt(i) + " appeared twice");
                    actual++;
                }
                store.close();
            }
            if (meta.areaCount() > 0) {
                SegmentStore store = SegmentStore.openReadOnly(
                        meta.directory().resolve(SegmentLoader.AREA_FILE));
                for (int i = 0; i < store.count(); i++) {
                    assertTrue(seenParcels.add(store.idAt(i)),
                            "parcel " + store.idAt(i) + " appeared twice");
                    actual++;
                }
                store.close();
            }
        }
        assertEquals(data.size(), actual);
    }

    @Test
    @DisplayName("every segment records bounds that cover its own data")
    void boundsCoverSegmentData() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 512 * 1024);
        List<SegmentLoader.SegmentMeta> metas = loader.load(mixed(1500, 700, 4));
        for (SegmentLoader.SegmentMeta meta : metas) {
            SegmentStore points = meta.pointCount() > 0
                    ? SegmentStore.openReadOnly(meta.directory().resolve(SegmentLoader.POINT_FILE))
                    : null;
            if (points != null) {
                for (int i = 0; i < points.count(); i++) {
                    assertTrue(meta.bounds().contains(points.xAt(i), points.yAt(i)),
                            "bounds of segment " + meta.segmentId() + " miss a point");
                }
                points.close();
            }
            SegmentStore areas = meta.areaCount() > 0
                    ? SegmentStore.openReadOnly(meta.directory().resolve(SegmentLoader.AREA_FILE))
                    : null;
            if (areas != null) {
                for (int i = 0; i < areas.count(); i++) {
                    BBox b = areas.bboxAt(i);
                    assertTrue(meta.bounds().contains(b.minX(), b.minY()));
                    assertTrue(meta.bounds().contains(b.maxX(), b.maxY()));
                }
                areas.close();
            }
        }
    }

    @Test
    @DisplayName("metadata survives a reload from disk")
    void metadataRoundTrip() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 512 * 1024);
        List<SegmentLoader.SegmentMeta> written = loader.load(mixed(1200, 600, 5));
        List<SegmentLoader.SegmentMeta> read = SegmentLoader.readMetas(tmp.resolve("idx"));
        assertEquals(written.size(), read.size());
        for (int i = 0; i < written.size(); i++) {
            assertEquals(written.get(i).pointCount(), read.get(i).pointCount());
            assertEquals(written.get(i).areaCount(), read.get(i).areaCount());
            assertEquals(written.get(i).segmentId(), read.get(i).segmentId());
        }
    }

    @Test
    @DisplayName("the external merge preserves every feature")
    void mergePreservesAllRows() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 256 * 1024);
        List<GeoFeature> data = TestData.points(4000, 6, 1000);
        List<SegmentLoader.SegmentMeta> metas = loader.load(data);
        assertTrue(metas.size() > 1, "need several segments to exercise the merge");
        SegmentLoader.SegmentMeta merged = loader.merge(metas, metas.size());
        assertEquals(4000, merged.pointCount());
        Set<Long> seen = new HashSet<>();
        SegmentStore store = SegmentStore.openReadOnly(
                merged.directory().resolve(SegmentLoader.POINT_FILE));
        for (int i = 0; i < store.count(); i++) {
            assertTrue(seen.add(store.idAt(i)), "duplicate after merge: " + store.idAt(i));
        }
        assertEquals(4000, seen.size());
        store.close();
    }

    @Test
    @DisplayName("merged rows are ordered by x")
    void mergeProducesSortedOutput() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 256 * 1024);
        List<SegmentLoader.SegmentMeta> metas = loader.load(TestData.points(3000, 7, 1000));
        SegmentLoader.SegmentMeta merged = loader.merge(metas, metas.size());
        SegmentStore store = SegmentStore.openReadOnly(
                merged.directory().resolve(SegmentLoader.POINT_FILE));
        for (int i = 1; i < store.count(); i++) {
            assertTrue(store.xAt(i - 1) <= store.xAt(i),
                    "merge output is not ordered by x at row " + i);
        }
        store.close();
    }

    @Test
    @DisplayName("the merge keeps polygon geometry intact")
    void mergePreservesPolygons() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 256 * 1024);
        List<GeoFeature> data = TestData.parcels(1200, 8, 1000);
        List<SegmentLoader.SegmentMeta> metas = loader.load(data);
        SegmentLoader.SegmentMeta merged = loader.merge(metas, metas.size());
        SegmentStore store = SegmentStore.openReadOnly(
                merged.directory().resolve(SegmentLoader.AREA_FILE));
        for (int i = 0; i < store.count(); i++) {
            assertArrayEqualsWithin(data, store);
        }
        store.close();
    }

    private static void assertArrayEqualsWithin(List<GeoFeature> data, SegmentStore store) {
        for (int i = 0; i < store.count(); i++) {
            GeoFeature f = store.featureAt(i);
            GeoFeature original = data.get((int) f.id());
            org.junit.jupiter.api.Assertions.assertArrayEquals(original.vertices(), f.vertices(),
                    "geometry of parcel " + f.id() + " was damaged by the merge");
        }
    }

    @Test
    @DisplayName("the plan honours the heap budget it is given")
    void planRespectsBudget() {
        for (long budget : new long[]{1024, 4096, 64 * 1024, 1024 * 1024}) {
            SegmentLoader.Plan plan = SegmentLoader.plan(1_000_000, SegmentStore.rowBytes(), budget);
            assertTrue(plan.rowsPerSegment() >= 1);
            assertTrue(plan.totalBytesPerSegment() <= budget + SegmentStore.rowBytes(),
                    "plan exceeds the budget at " + budget);
            assertEquals(1_000_000, (long) plan.rowsPerSegment() * plan.projectedSegments() >= 1_000_000
                    ? 1_000_000 : 0, "projection is inconsistent");
        }
    }

    @Test
    @DisplayName("an empty source produces no segments")
    void emptySource() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 1024 * 1024);
        assertTrue(loader.load(List.<GeoFeature>of()).isEmpty());
        assertThrows(IllegalArgumentException.class,
                () -> loader.merge(List.of(), 0));
    }

    @Test
    @DisplayName("segments reserve spare rows so single writes can append in place")
    void segmentsReserveHeadroom() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 64 * 1024 * 1024);
        List<GeoFeature> data = mixed(500, 200, 9);
        List<SegmentLoader.SegmentMeta> metas = loader.load(data);
        SegmentStore store = SegmentStore.openReadOnly(
                metas.get(0).directory().resolve(SegmentLoader.POINT_FILE));
        assertTrue(store.maxRows() > loader.rowsPerSegment(),
                "a full segment must still have room for an appended row");
        assertTrue(store.remainingRows() > 0);
        store.close();
    }

    @Test
    @DisplayName("loading is incremental: segment directories appear as it goes")
    void directoriesAreCreatedLazily() {
        Path root = tmp.resolve("idx");
        SegmentLoader loader = new SegmentLoader(root, 256 * 1024);
        List<SegmentLoader.SegmentMeta> metas = loader.load(mixed(2000, 1000, 10));
        assertTrue(metas.size() >= 2);
        for (SegmentLoader.SegmentMeta meta : metas) {
            assertTrue(Files.isDirectory(meta.directory()));
            assertTrue(Files.exists(meta.directory().resolve(SegmentLoader.META_FILE)));
        }
        assertFalse(metas.get(metas.size() - 1).isEmpty());
    }

    @Test
    @DisplayName("ids stay unique across a multi-segment load of distinct features")
    void uniqueIdsAcrossSegments() {
        SegmentLoader loader = new SegmentLoader(tmp.resolve("idx"), 256 * 1024);
        List<GeoFeature> data = new ArrayList<>();
        for (int i = 0; i < 5000; i++) {
            data.add(GeoFeature.point(i, FeatureType.PIPELINE_NODE, i % 100, i / 100));
        }
        List<SegmentLoader.SegmentMeta> metas = loader.load(data);
        Set<Long> ids = new HashSet<>();
        for (SegmentLoader.SegmentMeta meta : metas) {
            SegmentStore store = SegmentStore.openReadOnly(
                    meta.directory().resolve(SegmentLoader.POINT_FILE));
            for (int i = 0; i < store.count(); i++) {
                assertTrue(ids.add(store.idAt(i)));
            }
            store.close();
        }
        assertEquals(5000, ids.size());
    }
}
