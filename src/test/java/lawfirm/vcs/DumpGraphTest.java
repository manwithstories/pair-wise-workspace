package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class DumpGraphTest {

    private static final String NAME = "Law Firm IT";
    private static final String EMAIL = "it@firm.example";

    private static Identity identityAt(long offsetSeconds) {
        return Identity.of(NAME, EMAIL, 1700000000L + offsetSeconds, 480);
    }

    /** Builds a linear history of {@code n} commits, one file each. */
    private static List<ObjectId> linearHistory(Path root, int n) throws IOException {
        try (Repository repo = Repository.init(root)) {
            List<ObjectId> commits = new ArrayList<>();
            for (int i = 0; i < n; i++) {
                Files.writeString(root.resolve("f.txt"), "revision " + i);
                repo.add(root.resolve("f.txt"));
                commits.add(repo.commit("commit " + i, identityAt(i * 60L)));
            }
            return commits;
        }
    }

    @Test
    @DisplayName("the dump lists every commit with its parents and tree")
    void dumpDescribesEveryCommit(@TempDir Path root) throws IOException {
        List<ObjectId> commits = linearHistory(root, 4);
        try (Repository repo = Repository.open(root)) {
            String dump = new DumpGraph(repo).dump(repo.head());

            assertTrue(dump.contains("commit-graph 4 commit(s)"), dump);
            for (ObjectId c : commits) {
                assertTrue(dump.contains("commit " + c.hex()), "missing " + c.hex());
            }
            // the root commit reports no parent, every other commit exactly one
            assertTrue(dump.contains("parent -"), "root commit has no parent");
            assertTrue(dump.contains("parent " + commits.get(2).hex()), dump);
            // each commit reports its tree
            assertTrue(dump.contains("tree   " + repo.treeOf(commits.get(0)).hex()), dump);
            // and the subject
            assertTrue(dump.contains("subj   commit 3"), dump);

            // Parents come before their children, so the oldest commit is first.
            assertTrue(dump.indexOf("commit " + commits.get(0).hex())
                            < dump.indexOf("commit " + commits.get(3).hex()),
                    "the root commit must be emitted before its descendants");
        }
    }

    @Test
    @DisplayName("each line reports the file count and byte total of its tree")
    void dumpSummarisesTrees(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            Files.writeString(root.resolve("a.txt"), "12345");
            Files.writeString(root.resolve("b.txt"), "678");
            repo.addAll();
            repo.commit("two files", identityAt(0));

            String dump = new DumpGraph(repo).dump(repo.head());
            // 5 + 3 bytes of file content across 2 tracked paths
            assertTrue(dump.contains("files  2  bytes 8"), dump);
        }
    }

    @Test
    @DisplayName("topological order puts every parent before its children")
    void topologicalOrderRespectsAncestors(@TempDir Path root) throws IOException {
        List<ObjectId> commits = linearHistory(root, 6);
        try (Repository repo = Repository.open(root)) {
            List<DumpGraph.Node> nodes = new DumpGraph(repo).collect(repo.head());
            List<DumpGraph.Node> ordered = DumpGraph.inTopologicalOrder(nodes, repo.head());

            Map<ObjectId, Integer> position = new HashMap<>();
            for (int i = 0; i < ordered.size(); i++) {
                position.put(ordered.get(i).id(), i);
            }
            assertEquals(commits.size(), ordered.size());
            for (DumpGraph.Node n : ordered) {
                for (ObjectId p : n.parents()) {
                    Integer parentAt = position.get(p);
                    if (parentAt == null) continue;   // parent outside the collected set
                    assertTrue(parentAt < position.get(n.id()),
                            "parent " + p.abbrev(8) + " must precede " + n.id().abbrev(8));
                }
            }
        }
    }

    @Test
    @DisplayName("the dump of a tip contains the whole ancestry")
    void dumpFromTipCoversAncestry(@TempDir Path root) throws IOException {
        List<ObjectId> commits = linearHistory(root, 5);
        try (Repository repo = Repository.open(root)) {
            DumpGraph dumpGraph = new DumpGraph(repo);
            String fromHead = dumpGraph.dump(repo.head());
            String fromMiddle = dumpGraph.dump(commits.get(2));

            assertEquals("commit-graph 5 commit(s)", firstLine(fromHead));
            assertEquals("commit-graph 3 commit(s)", firstLine(fromMiddle),
                    "only the ancestors of the requested tip");
            assertFalse(fromMiddle.contains("commit " + commits.get(4).hex()));
        }
    }

    private static String firstLine(String text) {
        return text.lines().findFirst().orElse("");
    }

    @Test
    @DisplayName("an identity round trips through its canonical form")
    void identityRoundTrip() {
        Identity id = Identity.of("张三", "zhang@firm.example", 1700000000L, 480);
        Identity parsed = DumpGraph.parseIdentity(id.canonical());
        assertEquals(id.name(), parsed.name());
        assertEquals(id.email(), parsed.email());
        assertEquals(id.epochSecond(), parsed.epochSecond());
        assertEquals(id.tzOffsetMinutes(), parsed.tzOffsetMinutes());
        assertEquals("张三 <zhang@firm.example> 1700000000 +0800", id.canonical());
    }

    @Test
    @DisplayName("negative timezone offsets keep their sign")
    void negativeTimezoneRoundTrip() {
        Identity id = Identity.of("A User", "a@firm.example", 1700000000L, -300);
        assertEquals("A User <a@firm.example> 1700000000 -0500", id.canonical());
        Identity parsed = DumpGraph.parseIdentity(id.canonical());
        assertEquals(-300, parsed.tzOffsetMinutes());
    }

    @Test
    @DisplayName("a merge commit reports both parents")
    void mergeCommitReportsBothParents(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            Files.writeString(root.resolve("base.txt"), "base");
            repo.add(root.resolve("base.txt"));
            ObjectId base = repo.commit("base", identityAt(0));

            Files.writeString(root.resolve("left.txt"), "left");
            repo.add(root.resolve("left.txt"));
            ObjectId left = repo.commit("left side", identityAt(60));

            Files.writeString(root.resolve("right.txt"), "right");
            repo.add(root.resolve("right.txt"));
            ObjectId right = repo.commit("right side", identityAt(120));

            // Build a merge by hand so the second parent is explicit; the public
            // commit() helper only records HEAD as the parent.
            Files.writeString(root.resolve("merged.txt"), "merged");
            repo.add(root.resolve("merged.txt"));
            TreeBuilder merged = repo.index();
            ObjectId treeId = merged.persist(repo.looseStore());
            ObjectId merge = repo.looseStore().write(GitObject.commit(treeId,
                    List.of(left, right), identityAt(180).canonical(),
                    identityAt(180).canonical(), "merge both sides\n"));

            String dump = new DumpGraph(repo).dump(merge);
            // merge, left, right and base are all reachable from the merge tip
            assertTrue(dump.contains("commit-graph 4 commit(s)"), dump);
            assertTrue(dump.contains("parent " + left.hex()), dump);
            assertTrue(dump.contains("parent " + right.hex()), dump);
            assertTrue(dump.contains("subj   merge both sides"), dump);
        }
    }

    @Test
    @DisplayName("dump-graph output is stable across repeated runs")
    void dumpIsDeterministic(@TempDir Path root) throws IOException {
        linearHistory(root, 5);
        try (Repository repo = Repository.open(root)) {
            DumpGraph dumpGraph = new DumpGraph(repo);
            assertEquals(dumpGraph.dump(repo.head()), dumpGraph.dump(repo.head()));
        }
    }
}
