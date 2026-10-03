package com.citygis.spatial;

/** Immutable axis-aligned bounding box. */
public record BBox(double minX, double minY, double maxX, double maxY) {

    public BBox {
        if (minX > maxX || minY > maxY) {
            throw new IllegalArgumentException("degenerate bbox: " + minX + "," + minY
                    + " -> " + maxX + "," + maxY);
        }
    }

    public static BBox ofPoint(double x, double y) {
        return new BBox(x, y, x, y);
    }

    public static BBox envelope(double[] outline) {
        double minX = Double.POSITIVE_INFINITY;
        double minY = Double.POSITIVE_INFINITY;
        double maxX = Double.NEGATIVE_INFINITY;
        double maxY = Double.NEGATIVE_INFINITY;
        for (int i = 0; i < outline.length; i += 2) {
            minX = Math.min(minX, outline[i]);
            maxX = Math.max(maxX, outline[i]);
            minY = Math.min(minY, outline[i + 1]);
            maxY = Math.max(maxY, outline[i + 1]);
        }
        return new BBox(minX, minY, maxX, maxY);
    }

    public BBox union(BBox other) {
        return new BBox(Math.min(minX, other.minX), Math.min(minY, other.minY),
                Math.max(maxX, other.maxX), Math.max(maxY, other.maxY));
    }

    public boolean intersects(BBox other) {
        return minX <= other.maxX && maxX >= other.minX && minY <= other.maxY && maxY >= other.minY;
    }

    public boolean contains(double px, double py) {
        return px >= minX && px <= maxX && py >= minY && py <= maxY;
    }

    public double area() {
        return (maxX - minX) * (maxY - minY);
    }

    public double centerX() {
        return (minX + maxX) / 2.0;
    }

    public double centerY() {
        return (minY + maxY) / 2.0;
    }
}
