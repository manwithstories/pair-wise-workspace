package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Version parsing, SNAPSHOT handling and SemVer precedence ordering. */
class VersionTest {

    @Test
    @DisplayName("numeric segments compare numerically, not lexically")
    void numericOrdering() {
        assertTrue(Version.parse("1.10.0").compareTo(Version.parse("1.9.0")) > 0);
        assertTrue(Version.parse("2.0").compareTo(Version.parse("1.99.99")) > 0);
        assertTrue(Version.parse("1.0.0").compareTo(Version.parse("1.0")) == 0);
        assertTrue(Version.parse("1.02").compareTo(Version.parse("1.2")) == 0);
    }

    @Test
    @DisplayName("a release outranks any pre-release of the same numeric part")
    void releaseBeatsPrerelease() {
        assertTrue(Version.parse("1.2.3").compareTo(Version.parse("1.2.3-SNAPSHOT")) > 0);
        assertTrue(Version.parse("1.2.3-rc1").compareTo(Version.parse("1.2.3")) < 0);
    }

    @Test
    @DisplayName("prerelease identifiers follow SemVer precedence")
    void prereleaseOrdering() {
        assertTrue(Version.parse("1.0.0-alpha").compareTo(Version.parse("1.0.0-alpha.1")) < 0);
        assertTrue(Version.parse("1.0.0-alpha.1").compareTo(Version.parse("1.0.0-alpha.beta")) < 0);
        assertTrue(Version.parse("1.0.0-alpha.beta").compareTo(Version.parse("1.0.0-beta")) < 0);
        assertTrue(Version.parse("1.0.0-beta").compareTo(Version.parse("1.0.0-beta.2")) < 0);
        assertTrue(Version.parse("1.0.0-beta.2").compareTo(Version.parse("1.0.0-beta.11")) < 0);
        assertTrue(Version.parse("1.0.0-rc.1").compareTo(Version.parse("1.0.0")) < 0);
    }

    @Test
    @DisplayName("numeric prerelease identifiers rank below alphanumeric ones")
    void numericPrereleaseBelowAlnum() {
        assertTrue(Version.parse("1.0.0-1").compareTo(Version.parse("1.0.0-alpha")) < 0);
    }

    @Test
    @DisplayName("arbitrarily long numeric identifiers compare without overflow")
    void longNumericIdentifiers() {
        assertTrue(Version.parse("1.0.0-99999999999999999999")
                .compareTo(Version.parse("1.0.0-99999999999999999998")) > 0);
    }

    @Test
    @DisplayName("SNAPSHOT is recognised and sorts below its release")
    void snapshotRecognition() {
        assertTrue(Version.parse("1.2.3-SNAPSHOT").isSnapshot());
        assertTrue(Version.parse("1.2.3-snapshot").isSnapshot());
        assertFalse(Version.parse("1.2.3").isSnapshot());
        assertTrue(Version.parse("1.2.3-SNAPSHOT").isPreRelease());
        assertTrue(Version.parse("2.0-SNAPSHOT").isPreRelease());
    }

    @Test
    @DisplayName("segments beyond the third are kept and ordered")
    void extraSegments() {
        assertEquals(4, Version.parse("1.2.3.4").patch() > 0 ? 4 : 0);
        assertTrue(Version.parse("1.2.3.4").compareTo(Version.parse("1.2.3.3")) > 0);
        assertTrue(Version.parse("1.2.3").compareTo(Version.parse("1.2.3.0")) == 0);
    }

    @Test
    @DisplayName("rendering preserves what was parsed, since equal versions may differ in shape")
    void rendering() {
        assertEquals("1.2.0", Version.parse("1.2.0").toString());
        assertEquals("1.2.0-SNAPSHOT", Version.parse("1.2.0-SNAPSHOT").toString());
        // 1.2 and 1.2.0 compare equal but are not textually equal, so toString and raw report the
        // spelling that was parsed. Callers that need one canonical spelling must normalise themselves.
        assertEquals("1.2", Version.parse("1.2").toString());
        assertEquals("1.2.0", Version.parse("1.2.0").toString());
        assertEquals(0, Version.parse("1.2").compareTo(Version.parse("1.2.0")));
        assertEquals("1.2.0", Version.parse(" 1.2.0 ").raw());
    }

    @Test
    @DisplayName("equal versions share a hash so TreeMap lookups work")
    void equalityAndHashing() {
        Version a = Version.parse("1.02.0");
        Version b = Version.parse("1.2");
        assertEquals(a, b);
        assertEquals(a.hashCode(), b.hashCode());
        assertEquals(Version.parse("1.2.3-SNAPSHOT"), Version.parse("1.2.3-snapshot"));
    }

    @Test
    @DisplayName("sorting yields the documented order; equal versions may appear in either spelling")
    void sortOrder() {
        List<Version> vs = new ArrayList<>(List.of(
                Version.parse("2.0-SNAPSHOT"),
                Version.parse("1.9.9"),
                Version.parse("1.10"),
                Version.parse("1.10.0-rc1"),
                Version.parse("1.10.0"),
                Version.parse("1.2")));
        Collections.sort(vs);
        // 1.10 and 1.10.0 are the same version, so the sort keeps both and their relative order is not
        // specified. Compare by value and check that no element ever decreases.
        List<String> rendered = vs.stream().map(Version::toString).toList();
        assertEquals(6, rendered.size());
        for (int i = 1; i < vs.size(); i++) {
            assertTrue(vs.get(i - 1).compareTo(vs.get(i)) <= 0, "not sorted at index " + i);
        }
        assertEquals(Version.parse("1.2"), vs.get(0));
        assertEquals(Version.parse("1.9.9"), vs.get(1));
        assertEquals(Version.parse("1.10.0-rc1"), vs.get(2));
        assertEquals(Version.parse("1.10.0"), vs.get(3));
        assertEquals(Version.parse("2.0-SNAPSHOT"), vs.get(5));
    }

    @Test
    @DisplayName("malformed versions are rejected")
    void malformedRejected() {
        for (String bad : new String[]{"", "-", "1..2", "1.-2", "a.b", "1.2.", "1.2-", "1.2-SNAP SHOT",
                "1.2_3", "1.2.0.0.0a"}) {
            boolean threw;
            try {
                Version.parse(bad);
                threw = false;
            } catch (IllegalArgumentException e) {
                threw = true;
            }
            assertTrue(threw, "expected rejection of '" + bad + "'");
        }
    }

    @Test
    @DisplayName("a null version is rejected rather than silently treated as any")
    void nullRejected() {
        boolean threw;
        try {
            Version.parse(null);
            threw = false;
        } catch (IllegalArgumentException e) {
            threw = true;
        }
        assertTrue(threw);
    }
}
