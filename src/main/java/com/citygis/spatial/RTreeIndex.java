package com.citygis.spatial;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

/**
 * Off-heap R tree over the area (polygon) features of one {@link SegmentStore}.
 *
 * <p>Node links and node envelopes are kept in parallel primitive arrays, so the heap cost of the
 * tree is a fixed number of bytes per polygon and independent of the segment size. Bulk geometry is
 * read straight from the mapped store.
 *
 * <p>Layout is a balanced binary tree of {@code 2n-1} slots: slots {@code [0,n)} are leaves
 * pointing at store rows, slots {@code [n, 2n)} are internal nodes. Leaves are ordered by a
 * Z-order/Morton curve over their envelope centroids so that spatially adjacent polygons share
 * ancestors; internal envelopes are unions built bottom-up.
 *
 * <p>Range selection prunes on node envelopes. Polygon windows and point-in-polygon tests refine the
 * envelope result with an exact ray-cast so coarse envelopes never leak false positives.
 */
public final class RTreeIndex implements AutoCloseable {

    private static final int NO_CHILD = -1;

    /** Tolerance for orientation tests on projected city-scale coordinates. */
    private static final double EPS = 1e-9;

    private final SegmentStore store;
    private final int leafCount;
    private final int nodeCount;

    private final int[] slotToRow;   // leaves only; -1 for internal slots
    private final int[] leftChild;
    private final int[] rightChild;
    private final double[] envMinX;
    private final double[] envMinY;
    private final double[] envMaxX;
    private final double[] envMaxY;

    private int tombstones;

    public RTreeIndex(SegmentStore store) {
        this.store = store;
        this.leafCount = store.count();
        this.nodeCount = leafCount == 0 ? 0 : 2 * leafCount - 1;
        this.slotToRow = new int[nodeCount];
        this.leftChild = new int[nodeCount];
        this.rightChild = new int[nodeCount];
        this.envMinX = new double[nodeCount];
        this.envMinY = new double[nodeCount];
        this.envMaxX = new double[nodeCount];
        this.envMaxY = new double[nodeCount];
        Arrays.fill(leftChild, NO_CHILD);
        Arrays.fill(rightChild, NO_CHILD);
        Arrays.fill(slotToRow, -1);
        int dead = 0;
        for (int i = 0; i < leafCount; i++) {
            if (store.isTombstone(i)) {
                dead++;
            }
        }
        this.tombstones = dead;
        build(store);
    }

    public int size() {
        return leafCount;
    }

    public int liveCount() {
        return leafCount - tombstones;
    }

    public int tombstoneCount() {
        return tombstones;
    }

    public boolean isTombstone(int row) {
        return store.isTombstone(row);
    }

    /** Marks a row deleted. Node envelopes are left intact: stale envelopes only cost extra visits. */
    public boolean tombstone(int row) {
        if (row < 0 || row >= leafCount) {
            throw new IndexOutOfBoundsException("row " + row + " outside [0," + leafCount + ")");
        }
        if (store.tombstone(row)) {
            tombstones++;
            return true;
        }
        return false;
    }

    public BBox envelopeOf(int row) {
        return store.bboxAt(row);
    }

    /**
     * Range query returning every live polygon whose envelope intersects {@code window}.
     *
     * <p>This is the coarse filter; use {@link #searchContainingPoint} or
     * {@link #searchWithinPolygon} when the exact shape matters.
     */
    public List<GeoFeature> searchRange(BBox window) {
        List<GeoFeature> out = new ArrayList<>();
        if (nodeCount == 0) {
            return out;
        }
        if (root < 0) {
            return out;
        }
        int[] stack = new int[64];
        int sp = 0;
        stack[sp++] = root;
        while (sp > 0) {
            int slot = stack[--sp];
            if (envMinX[slot] > window.maxX() || envMaxX[slot] < window.minX()
                    || envMinY[slot] > window.maxY() || envMaxY[slot] < window.minY()) {
                continue;
            }
            if (slot < leafCount) {
                int row = slotToRow[slot];
                if (row >= 0 && !store.isTombstone(row) && store.bboxAt(row).intersects(window)) {
                    out.add(store.featureAt(row));
                }
                continue;
            }
            int l = leftChild[slot];
            int r = rightChild[slot];
            if (l == NO_CHILD) {
                continue;
            }
            if (sp + 2 > stack.length) {
                stack = Arrays.copyOf(stack, stack.length * 2);
            }
            stack[sp++] = l;
            stack[sp++] = r;
        }
        return out;
    }

    /**
     * Range query refined by an exact polygon test: returns live polygons whose geometry truly
     * overlaps the window outline (a vertex or edge hit, or full containment either way).
     */
    public List<GeoFeature> searchWithinPolygon(double[] window) {
        BBox envelope = BBox.envelope(window);
        List<GeoFeature> candidates = searchRange(envelope);
        List<GeoFeature> out = new ArrayList<>(candidates.size());
        for (GeoFeature f : candidates) {
            if (polygonsOverlap(f.vertices(), window)) {
                out.add(f);
            }
        }
        return out;
    }

    /** Returns live polygons that contain the given point, using envelope prune + ray cast. */
    public List<GeoFeature> searchContainingPoint(double px, double py) {
        List<GeoFeature> out = new ArrayList<>();
        if (nodeCount == 0) {
            return out;
        }
        if (root < 0) {
            return out;
        }
        int[] stack = new int[64];
        int sp = 0;
        stack[sp++] = root;
        while (sp > 0) {
            int slot = stack[--sp];
            if (px < envMinX[slot] || px > envMaxX[slot] || py < envMinY[slot] || py > envMaxY[slot]) {
                continue;
            }
            if (slot < leafCount) {
                int row = slotToRow[slot];
                if (row >= 0 && !store.isTombstone(row)) {
                    GeoFeature f = store.featureAt(row);
                    if (contains(f, px, py)) {
                        out.add(f);
                    }
                }
                continue;
            }
            int l = leftChild[slot];
            int r = rightChild[slot];
            if (l == NO_CHILD) {
                continue;
            }
            if (sp + 2 > stack.length) {
                stack = Arrays.copyOf(stack, stack.length * 2);
            }
            stack[sp++] = l;
            stack[sp++] = r;
        }
        return out;
    }

    /** Total number of slots (leaves + internal nodes). */
    public int nodeCount() {
        return nodeCount;
    }

    /** Slot id of the root, or -1 when the tree is empty. */
    public int rootSlot() {
        return root;
    }

    /** Envelope of an arbitrary node slot. */
    public BBox nodeEnvelope(int slot) {
        return new BBox(envMinX[slot], envMinY[slot], envMaxX[slot], envMaxY[slot]);
    }

    /** The store this tree was built over. */
    public SegmentStore store() {
        return store;
    }

    /**
     * Linear scan of every live row whose envelope meets {@code window}.
     *
     * <p>Used as the degraded path for very wide windows, where descending the tree costs more than
     * simply testing every envelope once.
     */
    public void scanWithin(SegmentStore store, BBox window, java.util.List<GeoFeature> sink) {
        for (int row = 0; row < leafCount; row++) {
            if (store.isTombstone(row)) {
                continue;
            }
            if (store.bboxAt(row).intersects(window)) {
                sink.add(store.featureAt(row));
            }
        }
    }

    /** Exact ray-cast point-in-polygon. */
    public static boolean contains(GeoFeature polygon, double px, double py) {
        double[] v = polygon.vertices();
        if (v == null) {
            return false;
        }
        boolean inside = false;
        int n = v.length / 2;
        for (int i = 0, j = n - 1; i < n; j = i++) {
            double xi = v[i * 2];
            double yi = v[i * 2 + 1];
            double xj = v[j * 2];
            double yj = v[j * 2 + 1];
            if ((yi > py) != (yj > py)
                    && px < (xj - xi) * (py - yi) / (yj - yi) + xi) {
                inside = !inside;
            }
        }
        return inside;
    }

    /** True when two closed polygons share at least one point. */
    public static boolean polygonsOverlap(double[] a, double[] b) {
        if (a == null || b == null) {
            return false;
        }
        if (containsCoords(a, b[0], b[1]) || containsCoords(b, a[0], a[1])) {
            return true;
        }
        int na = a.length / 2;
        int nb = b.length / 2;
        for (int i = 0, j = na - 1; i < na; j = i++) {
            for (int k = 0, l = nb - 1; k < nb; l = k++) {
                if (segmentsIntersect(a[j * 2], a[j * 2 + 1], a[i * 2], a[i * 2 + 1],
                        b[l * 2], b[l * 2 + 1], b[k * 2], b[k * 2 + 1])) {
                    return true;
                }
            }
        }
        return false;
    }

    private static boolean containsCoords(double[] poly, double px, double py) {
        boolean inside = false;
        int n = poly.length / 2;
        for (int i = 0, j = n - 1; i < n; j = i++) {
            double yi = poly[i * 2 + 1];
            double yj = poly[j * 2 + 1];
            if ((yi > py) != (yj > py)
                    && px < (poly[j * 2] - poly[i * 2]) * (py - yi) / (yj - yi) + poly[i * 2]) {
                inside = !inside;
            }
        }
        return inside;
    }

    /**
     * True when the closed segments {@code p1p2} and {@code p3p4} share at least one point.
     *
     * <p>Uses orientation tests with an explicit epsilon so that touching, collinear-overlapping and
     * vertex-on-edge configurations all count as intersections: for emergency dispatch, a parcel
     * that merely touches the selection band is inside it.
     */
    private static boolean segmentsIntersect(double x1, double y1, double x2, double y2,
                                            double x3, double y3, double x4, double y4) {
        double d1 = cross(x3, y3, x4, y4, x1, y1);
        double d2 = cross(x3, y3, x4, y4, x2, y2);
        double d3 = cross(x1, y1, x2, y2, x3, y3);
        double d4 = cross(x1, y1, x2, y2, x4, y4);
        if (((d1 > EPS && d2 < -EPS) || (d1 < -EPS && d2 > EPS))
                && ((d3 > EPS && d4 < -EPS) || (d3 < -EPS && d4 > EPS))) {
            return true; // proper crossing
        }
        if (Math.abs(d1) <= EPS && onSegment(x3, y3, x4, y4, x1, y1)) {
            return true;
        }
        if (Math.abs(d2) <= EPS && onSegment(x3, y3, x4, y4, x2, y2)) {
            return true;
        }
        if (Math.abs(d3) <= EPS && onSegment(x1, y1, x2, y2, x3, y3)) {
            return true;
        }
        if (Math.abs(d4) <= EPS && onSegment(x1, y1, x2, y2, x4, y4)) {
            return true;
        }
        return false;
    }

    private static boolean onSegment(double x1, double y1, double x2, double y2,
                                     double px, double py) {
        return px >= Math.min(x1, x2) - EPS && px <= Math.max(x1, x2) + EPS
                && py >= Math.min(y1, y2) - EPS && py <= Math.max(y1, y2) + EPS;
    }

    private static double cross(double ax, double ay, double bx, double by, double px, double py) {
        return (bx - ax) * (py - ay) - (by - ay) * (px - ax);
    }

    /** Streams the row index of every live polygon. */
    public void forEachLiveRow(java.util.function.Consumer<Integer> sink) {
        for (int slot = 0; slot < leafCount; slot++) {
            int row = slotToRow[slot];
            if (row >= 0 && !store.isTombstone(row)) {
                sink.accept(row);
            }
        }
    }

    // ---------------------------------------------------------------- build

    /**
     * Builds the tree in two passes:
     * <ol>
     *   <li>sort the leaf run by the Morton code of its envelope centroids, so neighbouring
     *       polygons end up under nearby internal nodes (this is what makes range queries
     *       sub-linear rather than a full scan);</li>
     *   <li>thread the sorted run into a balanced binary tree bottom-up, unioning envelopes as we
     *       go.</li>
     * </ol>
     */
    private void build(SegmentStore store) {
        if (leafCount == 0) {
            return;
        }
        int[] rows = new int[leafCount];
        long[] keys = new long[leafCount];
        double sumX = 0;
        double sumY = 0;
        for (int i = 0; i < leafCount; i++) {
            rows[i] = i;
            BBox b = store.bboxAt(i);
            sumX += b.centerX();
            sumY += b.centerY();
        }
        double cx = sumX / leafCount;
        double cy = sumY / leafCount;
        for (int i = 0; i < leafCount; i++) {
            BBox b = store.bboxAt(rows[i]);
            keys[i] = morton(b.centerX() - cx, b.centerY() - cy);
        }
        Integer[] order = new Integer[leafCount];
        for (int i = 0; i < leafCount; i++) {
            order[i] = i;
        }
        Arrays.sort(order, (a, b) -> {
            int c = Long.compareUnsigned(keys[a], keys[b]);
            return c != 0 ? c : Integer.compare(rows[a], rows[b]);
        });
        for (int i = 0; i < leafCount; i++) {
            int row = rows[order[i]];
            slotToRow[i] = row;
            BBox b = store.bboxAt(row);
            envMinX[i] = b.minX();
            envMinY[i] = b.minY();
            envMaxX[i] = b.maxX();
            envMaxY[i] = b.maxY();
        }

        nextInternal = leafCount;
        nextInternal = leafCount;
        root = linkLevel(0, leafCount);
    }

    /**
     * Links the contiguous slot range {@code [start,start+count)} into a balanced binary tree and
     * returns its root slot.
     *
     * <p>Children are built first and the parent envelope is the union of the two child envelopes —
     * computed after the recursion returns, never before.
     */
    private int linkLevel(int start, int count) {
        if (count == 1) {
            return start;
        }
        int mid = count / 2;
        int left = linkLevel(start, mid);
        int right = linkLevel(start + mid, count - mid);
        int node = nextInternal++;
        leftChild[node] = left;
        rightChild[node] = right;
        envMinX[node] = Math.min(envMinX[left], envMinX[right]);
        envMinY[node] = Math.min(envMinY[left], envMinY[right]);
        envMaxX[node] = Math.max(envMaxX[left], envMaxX[right]);
        envMaxY[node] = Math.max(envMaxY[left], envMaxY[right]);
        return node;
    }

    private int nextInternal;
    /** Slot id of the tree root; {@code -1} for an empty tree. */
    private int root = -1;

    /** 20-bit-per-axis Morton code; ample resolution for city-scale coordinates. */
    static long morton(double dx, double dy) {
        return interleave(quantize(dx)) | (interleave(quantize(dy)) << 1);
    }

    private static long quantize(double d) {
        long q = (long) Math.rint(d * 1024.0);
        if (q < 0) {
            q = 0;
        }
        if (q >= (1L << 20)) {
            q = (1L << 20) - 1;
        }
        return q;
    }

    private static long interleave(long v) {
        long out = 0;
        for (int i = 0; i < 20; i++) {
            out |= ((v >>> i) & 1L) << (2 * i);
        }
        return out;
    }

    @Override
    public void close() {
    }
}
