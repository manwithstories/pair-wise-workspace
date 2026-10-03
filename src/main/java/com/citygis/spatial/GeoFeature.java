package com.citygis.spatial;

/**
 * Immutable geographic element.
 *
 * <p>Instances are created by the ingest pipeline and handed to the indexes; they are never
 * mutated after publication, which lets the KD tree store them in off-heap mementos.
 */
public final class GeoFeature {

    private final long id;
    private final FeatureType type;
    private final double x;
    private final double y;
    private final double[] vertices;

    private GeoFeature(long id, FeatureType type, double x, double y, double[] vertices) {
        this.id = id;
        this.type = type;
        this.x = x;
        this.y = y;
        this.vertices = vertices;
    }

    public static GeoFeature point(long id, FeatureType type, double x, double y) {
        if (type == FeatureType.PARCEL) {
            throw new IllegalArgumentException("parcels must be created with an outline");
        }
        return new GeoFeature(id, type, x, y, null);
    }

    public static GeoFeature polygon(long id, FeatureType type, double[] outline) {
        if (outline == null || outline.length < 6 || outline.length % 2 != 0) {
            throw new IllegalArgumentException("outline must hold at least 3 x/y pairs");
        }
        double minX = Double.POSITIVE_INFINITY;
        double minY = Double.POSITIVE_INFINITY;
        for (int i = 0; i < outline.length; i += 2) {
            minX = Math.min(minX, outline[i]);
            minY = Math.min(minY, outline[i + 1]);
        }
        return new GeoFeature(id, type, minX, minY, outline.clone());
    }

    public long id() {
        return id;
    }

    public FeatureType type() {
        return type;
    }

    public double x() {
        return x;
    }

    public double y() {
        return y;
    }

    public double[] vertices() {
        return vertices == null ? null : vertices.clone();
    }

    /** True when this feature is an area element that owns an outline. */
    public boolean isArea() {
        return vertices != null;
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof GeoFeature other)) {
            return false;
        }
        return id == other.id && type == other.type
                && Double.compare(x, other.x) == 0 && Double.compare(y, other.y) == 0
                && java.util.Arrays.equals(vertices, other.vertices);
    }

    @Override
    public int hashCode() {
        int h = Long.hashCode(id);
        h = 31 * h + type.hashCode();
        h = 31 * h + Double.hashCode(x);
        h = 31 * h + Double.hashCode(y);
        h = 31 * h + java.util.Arrays.hashCode(vertices);
        return h;
    }

    @Override
    public String toString() {
        return "GeoFeature{id=" + id + ", type=" + type + ", x=" + x + ", y=" + y + '}';
    }
}
