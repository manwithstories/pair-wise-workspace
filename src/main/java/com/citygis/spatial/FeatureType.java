package com.citygis.spatial;

/** Kinds of geographic elements handled by the retrieval service. */
public enum FeatureType {
    /** Point of interest. Participates in the KD tree (k-nearest-neighbour). */
    POI,
    /** Parcel / land-cover polygon. Participates in the R tree (range query + containment). */
    PARCEL,
    /** Pipeline node. Participates in the KD tree. */
    PIPELINE_NODE
}
