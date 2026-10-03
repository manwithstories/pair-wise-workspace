package com.citygis.spatial;

/** Mutable 2D point used as the query probe for k-nearest-neighbour lookups. */
public record GeoPoint(double x, double y) {
    public double distanceTo(double ox, double oy) {
        double dx = x - ox;
        double dy = y - oy;
        return Math.sqrt(dx * dx + dy * dy);
    }
}
