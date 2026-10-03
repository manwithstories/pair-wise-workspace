package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Version parsing, normalisation and ordering. */
class VersionTest {

    @Test
    @DisplayName("trailing zero padding is normalised away")
    void normalisesTrailingZeros() {
        assertEquals(Version.parse("1.2"), Version.parse("1.2.0"));
        assertEquals(Version.parse("1.2"), Version.parse("1.2.0.0"));
        assertEquals("1.2", Version.parse("1.2.0").toString());
        // A release of all zeros must not collapse to nothing.
        assertEquals(Version.parse("0"), Version.parse("0.0"));
    }

    @Test
    @DisplayName("numeric components compare component-wise, missing ones as zero")
    void ordersNumericComponents() {
        assertTrue(Version.parse("1.2").compareTo(Version.parse("1.2.1")) < 0);
        assertTrue(Version.parse("1.10").compareTo(Version.parse("1.9")) > 0,
                "components compare numerically, not lexicographically");
        assertTrue(Version.parse("2.0").compareTo(Version.parse("1.99.99")) > 0);
        assertEquals(0, Version.parse("1.0").compareTo(Version.parse("1.0.0")));
    }

    @Test
    @DisplayName("a release outranks any pre-release of the same release")
    void releaseBeatsPreRelease() {
        assertTrue(Version.parse("1.0-RC1").compareTo(Version.parse("1.0")) < 0);
        assertTrue(Version.parse("1.0-alpha").compareTo(Version.parse("1.0-beta")) < 0);
        assertTrue(Version.parse("1.0").compareTo(Version.parse("1.0-rc1")) > 0);
    }

    @Test
    @DisplayName("a numeric qualifier ranks below a non-numeric one")
    void numericQualifierRanksBelowAlphabetic() {
        assertTrue(Version.parse("1.0-1").compareTo(Version.parse("1.0-alpha")) < 0);
        assertTrue(Version.parse("1.0-2").compareTo(Version.parse("1.0-10")) < 0,
                "numeric qualifiers compare by value, not text");
    }

    @Test
    @DisplayName("a shorter qualifier list wins on an otherwise equal prefix")
    void shorterQualifierListWins() {
        assertTrue(Version.parse("1.0-RC1").compareTo(Version.parse("1.0-RC1.1")) < 0);
        assertEquals(Version.parse("1.0-RC1"), Version.parse("1.0-RC1"));
    }

    @Test
    @DisplayName("SNAPSHOT ranks below the released form and equals itself")
    void snapshotOrdering() {
        assertTrue(Version.parse("1.0-SNAPSHOT").compareTo(Version.parse("1.0")) < 0);
        assertEquals(Version.parse("1.0-SNAPSHOT"), Version.parse("1.0-SNAPSHOT"));
        assertFalse(Version.parse("1.0-SNAPSHOT").isPreRelease(),
                "SNAPSHOT is a marker, not a pre-release qualifier");
        assertTrue(Version.parse("1.0-SNAPSHOT").isSnapshot());
        assertEquals("1-SNAPSHOT", Version.parse("1.0-SNAPSHOT").toString(),
                "trailing zeros normalise away in the canonical form");
    }

    @Test
    @DisplayName("a qualifier ending in the letters SNAPSHOT is not the marker")
    void doesNotMistakeQualifierForMarker() {
        Version v = Version.parse("1.0-NOTASNAPSHOT");
        assertFalse(v.isSnapshot(), "'NOTASNAPSHOT' must not be read as the SNAPSHOT marker");
        assertTrue(v.isPreRelease());
        assertEquals(Version.parse("1.0-NOTASNAPSHOT"), v);
    }

    @Test
    @DisplayName("a timestamped snapshot qualifier is preserved")
    void parsesTimestampedSnapshot() {
        Version v = Version.parse("1.0-20240101.101500-3-SNAPSHOT");
        assertTrue(v.isSnapshot());
        assertEquals("1-20240101.101500-3-SNAPSHOT", v.toString());
        assertTrue(v.compareTo(Version.parse("1.0")) < 0);
    }

    @Test
    @DisplayName("sorting a mixed list yields a stable total order")
    void sortsMixedList() {
        List<Version> in = new ArrayList<>(Arrays.asList(
                Version.parse("1.0"), Version.parse("1.0-SNAPSHOT"), Version.parse("1.0-RC1"),
                Version.parse("0.9"), Version.parse("1.0.1"), Version.parse("2.0-beta")));
        in.sort(null);
        List<String> got = new ArrayList<>();
        for (Version v : in) {
            got.add(v.toString());
        }
        assertEquals(List.of("0.9", "1-RC1", "1-SNAPSHOT", "1", "1.0.1", "2-beta"), got);
    }

    @Test
    @DisplayName("equals and hashCode agree on the normalised form")
    void equalityMatchesNormalisation() {
        assertEquals(Version.parse("1.2.0"), Version.parse("1.2"));
        assertEquals(Version.parse("1.2.0").hashCode(), Version.parse("1.2").hashCode());
        assertFalse(Version.parse("1.2").equals(Version.parse("1.2-SNAPSHOT")));
        assertFalse(Version.parse("1.2").equals(null));
        assertFalse(Version.parse("1.2").equals("1.2"));
    }

    @Test
    @DisplayName("malformed versions are rejected with a clear message")
    void rejectsMalformedVersions() {
        assertThrows(IllegalArgumentException.class, () -> Version.parse(null));
        assertThrows(IllegalArgumentException.class, () -> Version.parse(""));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("   "));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("1.2 3"));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("abc"));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("1.x"));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("1..2"));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("-RC1"));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("1.2-RC1."));
        assertThrows(IllegalArgumentException.class, () -> Version.parse("1.999999999999"));
    }

    @Test
    @DisplayName("component access pads missing components with zero")
    void componentAccess() {
        Version v = Version.parse("1.2");
        assertEquals(1, v.component(0));
        assertEquals(2, v.component(1));
        assertEquals(0, v.component(2), "missing components read as zero");
        assertEquals(2, v.releaseWidth());
    }
}
