package com.citygis.spatial.demo;

import com.citygis.spatial.BBox;
import com.citygis.spatial.FeatureType;
import com.citygis.spatial.GeoFeature;
import com.citygis.spatial.GeoPoint;
import com.citygis.spatial.Neighbor;
import com.citygis.spatial.SpatialIndex;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Random;
import java.util.Scanner;

/**
 * Terminal front end for dispatchers: ingests a synthetic city data set and runs nearest-neighbour,
 * range and containment queries against it.
 *
 * <p>Run with {@code java -Xmx512m -cp target/classes com.citygis.spatial.demo.DispatchConsole}.
 */
public final class DispatchConsole {

    private static final long HEAP_BUDGET = 128L * 1024 * 1024;

    private DispatchConsole() {
    }

    public static void main(String[] args) throws Exception {
        Path root = args.length > 0 ? Path.of(args[0])
                : Files.createTempDirectory("spatial-demo");
        int pointCount = args.length > 1 ? Integer.parseInt(args[1]) : 200_000;
        int parcelCount = args.length > 2 ? Integer.parseInt(args[2]) : 50_000;

        System.out.println("City natural-resource spatial retrieval — dispatch console");
        System.out.println("  data root  : " + root);
        System.out.printf("  heap budget: %d MB (process max %d MB)%n",
                HEAP_BUDGET / (1024 * 1024), Runtime.getRuntime().maxMemory() / (1024 * 1024));
        System.out.printf("  ingesting %,d POIs and %,d parcels...%n", pointCount, parcelCount);

        long t0 = System.nanoTime();
        try (SpatialIndex index = SpatialIndex.build(root, synthetic(pointCount, parcelCount),
                HEAP_BUDGET)) {
            long buildMs = (System.nanoTime() - t0) / 1_000_000;
            System.out.printf("  indexed in %d ms across %d segments (%,d features)%n%n",
                    buildMs, index.segmentCount(), index.featureCount());

            try (Scanner in = new Scanner(System.in)) {
                boolean running = true;
                while (running) {
                    System.out.print("knn <x> <y> [k] | range <x1> <y1> <x2> <y2> "
                            + "| parcel <x> <y> | stats | quit > ");
                    System.out.flush();
                    String line = in.nextLine().trim();
                    if (line.isEmpty()) {
                        continue;
                    }
                    String[] parts = line.split("\\s+");
                    switch (parts[0].toLowerCase(Locale.ROOT)) {
                        case "knn" -> knn(index, parts);
                        case "range" -> range(index, parts);
                        case "parcel" -> parcel(index, parts);
                        case "stats" -> stats(index);
                        case "quit", "exit" -> running = false;
                        default -> System.out.println("  unknown command: " + parts[0]);
                    }
                }
            }
        }
        System.out.println("done.");
    }

    private static void knn(SpatialIndex index, String[] p) {
        double x = Double.parseDouble(p[1]);
        double y = Double.parseDouble(p[2]);
        int k = p.length > 3 ? Integer.parseInt(p[3]) : 10;
        long t0 = System.nanoTime();
        List<Neighbor> hits = index.knn(new GeoPoint(x, y), k);
        long us = (System.nanoTime() - t0) / 1000;
        System.out.printf("  %d nearest to (%.2f, %.2f) in %d us%n", hits.size(), x, y, us);
        int n = 0;
        for (Neighbor h : hits) {
            System.out.printf("    %2d. id=%-10d %-14s d=%8.2f  (%.2f, %.2f)%n",
                    ++n, h.id(), h.feature().type(), h.distance(),
                    h.feature().x(), h.feature().y());
        }
    }

    private static void range(SpatialIndex index, String[] p) {
        BBox w = new BBox(Double.parseDouble(p[1]), Double.parseDouble(p[2]),
                Double.parseDouble(p[3]), Double.parseDouble(p[4]));
        long t0 = System.nanoTime();
        List<GeoFeature> hits = index.rangeSearch(w);
        long us = (System.nanoTime() - t0) / 1000;
        System.out.printf("  %,d parcels intersect %s in %d us%n", hits.size(), w, us);
        for (int i = 0; i < Math.min(10, hits.size()); i++) {
            System.out.printf("    id=%d%n", hits.get(i).id());
        }
        if (hits.size() > 10) {
            System.out.println("    ... and " + (hits.size() - 10) + " more");
        }
    }

    private static void parcel(SpatialIndex index, String[] p) {
        double x = Double.parseDouble(p[1]);
        double y = Double.parseDouble(p[2]);
        List<GeoFeature> hits = index.containingParcels(x, y);
        System.out.printf("  %,d parcels contain (%.2f, %.2f)%n", hits.size(), x, y);
        for (GeoFeature f : hits) {
            System.out.printf("    id=%d%n", f.id());
        }
    }

    private static void stats(SpatialIndex index) {
        SpatialIndex.Generation g = index.generation();
        System.out.printf("  segments=%d features=%,d heapUsed=%d MB%n",
                g.segmentCount(), g.featureCount(),
                (Runtime.getRuntime().totalMemory() - Runtime.getRuntime().freeMemory())
                        / (1024 * 1024));
        for (int i = 0; i < Math.min(5, g.segmentCount()); i++) {
            System.out.printf("    seg %d: points=%,d areas=%,d bounds=%s%n",
                    i, g.segments().get(i).pointCount(), g.segments().get(i).areaCount(),
                    g.segments().get(i).bounds());
        }
        if (g.segmentCount() > 5) {
            System.out.println("    ... " + (g.segmentCount() - 5) + " more segments");
        }
    }

    /** Builds a synthetic city data set: POIs, pipeline nodes and parcels over a 1000x1000 area. */
    private static List<GeoFeature> synthetic(int points, int parcels) {
        Random r = new Random(20260904L);
        List<GeoFeature> out = new ArrayList<>(points + parcels);
        for (int i = 0; i < points; i++) {
            double x = r.nextDouble() * 1000;
            double y = r.nextDouble() * 1000;
            GeoFeature f = (i % 4 == 0)
                    ? GeoFeature.point(i, FeatureType.PIPELINE_NODE, x, y)
                    : GeoFeature.point(i, FeatureType.POI, x, y);
            out.add(f);
        }
        for (int i = 0; i < parcels; i++) {
            double x = r.nextDouble() * 1000;
            double y = r.nextDouble() * 1000;
            double s = 5 + r.nextDouble() * 25;
            out.add(GeoFeature.polygon(points + i, FeatureType.PARCEL,
                    new double[]{x, y, x + s, y, x + s, y + s, x, y + s}));
        }
        return out;
    }
}
