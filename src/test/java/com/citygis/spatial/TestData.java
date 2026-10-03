package com.citygis.spatial;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Random;

/** Deterministic data generators and brute-force oracles shared by the tests. */
final class TestData {

    private TestData() {
    }

    /** Uniform random points in {@code [0,extent]x[0,extent]}. */
    static List<GeoFeature> points(int count, long seed, double extent) {
        Random r = new Random(seed);
        List<GeoFeature> out = new ArrayList<>(count);
        for (int i = 0; i < count; i++) {
            out.add(GeoFeature.point(i, FeatureType.POI, r.nextDouble() * extent,
                    r.nextDouble() * extent));
        }
        return out;
    }

    /** Axis-aligned square parcels of random size and position. */
    static List<GeoFeature> parcels(int count, long seed, double extent) {
        Random r = new Random(seed);
        List<GeoFeature> out = new ArrayList<>(count);
        for (int i = 0; i < count; i++) {
            double x = r.nextDouble() * extent;
            double y = r.nextDouble() * extent;
            double s = 5 + r.nextDouble() * 20;
            double[] v = {x, y, x + s, y, x + s, y + s, x, y + s};
            out.add(GeoFeature.polygon(i, FeatureType.PARCEL, v));
        }
        return out;
    }

    /**
     * Exact k-nearest-neighbour oracle: a full linear scan with the same total order the index uses.
     *
     * <p>Sorting by (distance, id) makes the expected answer unique even when several features are
     * equidistant from the probe, so the comparison is exact rather than approximate.
     */
    static List<Neighbor> bruteForceKnn(List<GeoFeature> features, GeoPoint probe, int k,
                                        List<Long> deleted) {
        List<Neighbor> all = new ArrayList<>(features.size());
        for (GeoFeature f : features) {
            if (deleted != null && deleted.contains(f.id())) {
                continue;
            }
            all.add(new Neighbor(f, probe.distanceTo(f.x(), f.y())));
        }
        all.sort(Comparator.naturalOrder());
        return new ArrayList<>(all.subList(0, Math.min(k, all.size())));
    }

    /** Exact set of feature ids whose envelope meets the window. */
    static List<Long> bruteForceRange(List<GeoFeature> parcels, BBox window) {
        List<Long> out = new ArrayList<>();
        for (GeoFeature f : parcels) {
            if (BBox.envelope(f.vertices()).intersects(window)) {
                out.add(f.id());
            }
        }
        out.sort(Long::compareTo);
        return out;
    }

    /** Exact set of feature ids containing the point. */
    static List<Long> bruteForceContaining(List<GeoFeature> parcels, double x, double y) {
        List<Long> out = new ArrayList<>();
        for (GeoFeature f : parcels) {
            if (RTreeIndex.contains(f, x, y)) {
                out.add(f.id());
            }
        }
        out.sort(Long::compareTo);
        return out;
    }

    /** Sorted ids of a feature list, for order-insensitive comparison. */
    static List<Long> ids(List<GeoFeature> features) {
        List<Long> out = new ArrayList<>(features.size());
        for (GeoFeature f : features) {
            out.add(f.id());
        }
        out.sort(Long::compareTo);
        return out;
    }

    /** Sorted ids of a neighbour list. */
    static List<Long> neighborIds(List<Neighbor> neighbors) {
        List<Long> out = new ArrayList<>(neighbors.size());
        for (Neighbor n : neighbors) {
            out.add(n.id());
        }
        out.sort(Long::compareTo);
        return out;
    }

    static List<GeoFeature> merge(List<GeoFeature> a, List<GeoFeature> b) {
        List<GeoFeature> out = new ArrayList<>(a.size() + b.size());
        out.addAll(a);
        out.addAll(b);
        return out;
    }
}
