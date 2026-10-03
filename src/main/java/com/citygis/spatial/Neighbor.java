package com.citygis.spatial;

/**
 * One k-nearest-neighbour hit.
 *
 * <p>The natural ordering is by ascending distance. Ties are broken by ascending feature id so that
 * the parallel merge and a single-threaded scan always agree on the same Top-K set and order; see
 * {@link SpatialIndex#knn}.
 */
public record Neighbor(GeoFeature feature, double distance) implements Comparable<Neighbor> {

    public long id() {
        return feature.id();
    }

    /** Ascending distance, then ascending id — the canonical Top-K order. */
    @Override
    public int compareTo(Neighbor other) {
        int byDistance = Double.compare(distance, other.distance);
        return byDistance != 0 ? byDistance : Long.compare(feature.id(), other.feature.id());
    }
}
