package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.List;
import java.util.Random;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** Correctness of the area R tree: range selection, containment and exact geometry. */
class RTreeIndexTest {

    @TempDir
    Path tmp;

    private RTreeIndex build(List<GeoFeature> features) {
        int pool = 0;
        for (GeoFeature f : features) {
            pool += SegmentStore.vertexSlots(f);
        }
        SegmentStore store = SegmentStore.create(tmp.resolve("a.seg"),
                Math.max(1, features.size()), Math.max(1, pool + 16));
        for (GeoFeature f : features) {
            store.append(f);
        }
        store.flushHeader();
        return new RTreeIndex(store);
    }

    @Test
    @DisplayName("rectangle range query matches an exhaustive envelope scan")
    void rangeMatchesBruteForce() {
        List<GeoFeature> data = TestData.parcels(1500, 3, 1000);
        RTreeIndex rt = build(data);
        Random r = new Random(77);
        for (int t = 0; t < 60; t++) {
            double x = r.nextDouble() * 800;
            double y = r.nextDouble() * 800;
            BBox window = new BBox(x, y, x + 200, y + 200);
            assertEquals(TestData.bruteForceRange(data, window), TestData.ids(rt.searchRange(window)),
                    "window=" + window);
        }
    }

    @Test
    @DisplayName("point-in-polygon query matches an exhaustive ray-cast scan")
    void containmentMatchesBruteForce() {
        List<GeoFeature> data = TestData.parcels(1200, 5, 1000);
        RTreeIndex rt = build(data);
        Random r = new Random(88);
        for (int t = 0; t < 60; t++) {
            double x = r.nextDouble() * 1000;
            double y = r.nextDouble() * 1000;
            assertEquals(TestData.bruteForceContaining(data, x, y),
                    TestData.ids(rt.searchContainingPoint(x, y)), "point=" + x + "," + y);
        }
    }

    @Test
    @DisplayName("the degraded linear scan returns exactly what the tree returns")
    void degradedScanMatchesTree() {
        List<GeoFeature> data = TestData.parcels(800, 13, 1000);
        RTreeIndex rt = build(data);
        BBox window = new BBox(100, 100, 900, 900);
        List<GeoFeature> scanned = new java.util.ArrayList<>();
        rt.scanWithin(rt.store(), window, scanned);
        assertEquals(TestData.bruteForceRange(data, window), TestData.ids(scanned));
        assertEquals(TestData.bruteForceRange(data, window), TestData.ids(rt.searchRange(window)));
    }

    @Test
    @DisplayName("node envelopes prune correctly: the root covers every leaf")
    void rootEnvelopeCoversAllLeaves() {
        List<GeoFeature> data = TestData.parcels(500, 17, 400);
        RTreeIndex rt = build(data);
        BBox root = rt.nodeEnvelope(rt.rootSlot());
        for (GeoFeature f : data) {
            BBox b = BBox.envelope(f.vertices());
            assertTrue(root.contains(b.minX(), b.minY()), "root misses min corner of " + f.id());
            assertTrue(root.contains(b.maxX(), b.maxY()), "root misses max corner of " + f.id());
        }
    }

    @Test
    @DisplayName("polygon windows use the exact overlap test, not just envelopes")
    void polygonWindowUsesExactGeometry() {
        // A thin window that clips parcels without containing their centres.
        List<GeoFeature> data = new java.util.ArrayList<>();
        for (int i = 0; i < 40; i++) {
            data.add(GeoFeature.polygon(i, FeatureType.PARCEL,
                    new double[]{i * 10.0, 0, i * 10.0 + 9, 0, i * 10.0 + 9, 10, i * 10.0, 10}));
        }
        RTreeIndex rt = build(data);
        // Window band spans x in [95,115]; parcel i spans [10i, 10i+9].
        double[] window = {95, -5, 115, 15};
        List<GeoFeature> got = rt.searchWithinPolygon(window);
        // Parcels 10 (100..109) and 11 (110..119) sit inside the band.
        assertTrue(got.stream().anyMatch(f -> f.id() == 10), "parcel 10 overlaps the band");
        assertTrue(got.stream().anyMatch(f -> f.id() == 11), "parcel 11 overlaps the band");
        // Parcel 12 starts at x=120, past the band, so its envelope must be rejected.
        assertFalse(got.stream().anyMatch(f -> f.id() == 12), "parcel 12 starts past the band");
        // Every returned parcel must pass the exact overlap test, never just the envelope test.
        for (GeoFeature f : got) {
            assertTrue(RTreeIndex.polygonsOverlap(f.vertices(), window),
                    "parcel " + f.id() + " does not actually overlap the window");
        }
    }

    @Test
    @DisplayName("ray-cast containment handles inside, outside and vertices")
    void rayCast() {
        double[] square = {0, 0, 10, 0, 10, 10, 0, 10};
        GeoFeature f = GeoFeature.polygon(1, FeatureType.PARCEL, square);
        assertTrue(RTreeIndex.contains(f, 5, 5), "centre is inside");
        assertTrue(RTreeIndex.contains(f, 0.5, 0.5), "near-corner is inside");
        assertFalse(RTreeIndex.contains(f, 15, 5), "right of the square is outside");
        assertFalse(RTreeIndex.contains(f, 5, -1), "below the square is outside");
        assertTrue(RTreeIndex.contains(f, 100, 100) == false, "far outside");
    }

    @Test
    @DisplayName("tombstoned polygons vanish from range and containment results")
    void tombstoneHidesParcels() {
        List<GeoFeature> data = TestData.parcels(300, 23, 500);
        RTreeIndex rt = build(data);
        BBox window = new BBox(0, 0, 500, 500);
        List<Long> before = TestData.bruteForceRange(data, window);
        assertEquals(before, TestData.ids(rt.searchRange(window)));
        List<Long> deleted = new java.util.ArrayList<>();
        for (int i = 0; i < 100; i++) {
            rt.tombstone(i);
            deleted.add((long) i);
        }
        assertEquals(200, rt.liveCount());
        List<Long> expected = new java.util.ArrayList<>();
        for (GeoFeature f : data) {
            if (!deleted.contains(f.id()) && BBox.envelope(f.vertices()).intersects(window)) {
                expected.add(f.id());
            }
        }
        expected.sort(Long::compareTo);
        assertEquals(expected, TestData.ids(rt.searchRange(window)));
    }

    @Test
    @DisplayName("an empty tree answers every query without error")
    void emptyTree() {
        RTreeIndex rt = build(List.of());
        assertEquals(0, rt.size());
        assertTrue(rt.searchRange(new BBox(0, 0, 1, 1)).isEmpty());
        assertTrue(rt.searchContainingPoint(0, 0).isEmpty());
    }

    @Test
    @DisplayName("a single parcel and single-point queries behave")
    void tinyTree() {
        RTreeIndex rt = build(List.of(GeoFeature.polygon(1, FeatureType.PARCEL,
                new double[]{0, 0, 4, 0, 4, 4, 0, 4})));
        assertEquals(List.of(1L), TestData.ids(rt.searchContainingPoint(2, 2)));
        assertEquals(List.of(), TestData.ids(rt.searchContainingPoint(9, 9)));
        assertEquals(List.of(1L), TestData.ids(rt.searchRange(new BBox(0, 0, 4, 4))));
        assertEquals(List.of(), TestData.ids(rt.searchRange(new BBox(10, 10, 20, 20))));
    }

    @Test
    @DisplayName("tree size is twice the leaf count minus one and the root is an internal node")
    void topology() {
        RTreeIndex rt = build(TestData.parcels(64, 29, 200));
        assertEquals(64, rt.size());
        assertEquals(127, rt.nodeCount());
        assertTrue(rt.rootSlot() >= 64, "root must be an internal node");
    }
}
