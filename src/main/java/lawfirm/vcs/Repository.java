package lawfirm.vcs;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.FileVisitResult;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.SimpleFileVisitor;
import java.nio.file.attribute.BasicFileAttributes;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.HashSet;
import java.util.Map;
import java.util.Set;
import java.util.stream.Stream;

/**
 * A git-compatible repository with content defined storage for large blobs.
 *
 * <p>Layout under the repository's {@code .vcs} directory:
 *
 * <pre>
 *   .vcs/HEAD              "ref: refs/heads/main"
 *   .vcs/refs/heads/main   current commit id
 *   .vcs/refs/stage        staged root tree id
 *   .vcs/log               one line per commit: id, date, subject
 *   .vcs/objects/xx/yyy…   deflated loose blobs, trees and commits
 *   .vcs/pack/…            Rabin chunk pool and copy/insert recipes
 * </pre>
 *
 * <p>Every object id produced here is the SHA-1 native git computes for the same
 * content, so a commit made here can be audited by the firm's existing git
 * toolchain and vice versa. {@link #exportLooseObject} makes that literal: it
 * writes objects in git's own on-disk format, which {@code git fsck} accepts.
 */
public final class Repository implements AutoCloseable {

    public static final String DIRECTORY = ".vcs";
    public static final String DEFAULT_BRANCH = "main";

    /** Blobs at or below this size are stored as ordinary loose git objects. */
    public static final int LOOSE_BLOB_LIMIT = 1024 * 1024;

    private final Path root;
    private final Path dir;
    private final GitObject.LooseStore loose;
    private final ChunkPack pack;
    private final RabinChunker chunker;
    private TreeBuilder index;

    private Repository(Path root, RabinChunker chunker) throws IOException {
        this.root = root;
        this.dir = root.resolve(DIRECTORY);
        this.loose = new GitObject.LooseStore(dir.resolve("objects"));
        this.chunker = chunker;
        this.pack = new ChunkPack(dir.resolve("pack"), chunker);
        this.index = loadIndex();
    }

    /** Creates a new repository, or opens the existing one at {@code root}. */
    public static Repository init(Path root) throws IOException {
        return init(root, new RabinChunker());
    }

    public static Repository init(Path root, RabinChunker chunker) throws IOException {
        Files.createDirectories(root);
        Path dir = root.resolve(DIRECTORY);
        if (!Files.isDirectory(dir)) {
            Files.createDirectories(dir.resolve("objects"));
            Files.createDirectories(dir.resolve("refs/heads"));
            Files.createDirectories(dir.resolve("pack"));
            Files.writeString(dir.resolve("HEAD"), "ref: refs/heads/" + DEFAULT_BRANCH + "\n");
            Files.writeString(dir.resolve("config"),
                    "[core]\n\trepositoryformatversion = 0\n\tbare = false\n");
        }
        return new Repository(root, chunker);
    }

    public static Repository open(Path root) throws IOException {
        if (!Files.isDirectory(root.resolve(DIRECTORY))) {
            throw new IOException("not a repository: " + root);
        }
        return init(root);
    }

    public Path root() {
        return root;
    }

    public Path directory() {
        return dir;
    }

    public ChunkPack pack() {
        return pack;
    }

    public RabinChunker chunker() {
        return chunker;
    }

    public GitObject.LooseStore looseStore() {
        return loose;
    }

    // ------------------------------------------------------------- refs

    private Path refPath(String name) {
        return dir.resolve("refs").resolve(name);
    }

    /** The commit id HEAD points at, or null on an unborn branch. */
    public ObjectId head() throws IOException {
        String headText = Files.readString(dir.resolve("HEAD")).trim();
        if (headText.startsWith("ref: ")) {
            Path ref = refPath(headText.substring(5).trim());
            if (!Files.isRegularFile(ref)) return null;
            return ObjectId.fromHex(Files.readString(ref).trim());
        }
        return ObjectId.fromHex(headText);
    }

    public String branch() throws IOException {
        String headText = Files.readString(dir.resolve("HEAD")).trim();
        return headText.startsWith("ref: ") ? headText.substring(5).trim() : "(detached)";
    }

    private void setHead(ObjectId commit) throws IOException {
        String headText = Files.readString(dir.resolve("HEAD")).trim();
        if (headText.startsWith("ref: ")) {
            Path ref = refPath(headText.substring(5).trim());
            Files.createDirectories(ref.getParent());
            Files.writeString(ref, commit.hex() + "\n");
        } else {
            Files.writeString(dir.resolve("HEAD"), commit.hex() + "\n");
        }
    }

    /** Resolves a revision: full id, "HEAD", or "HEAD~n". */
    public ObjectId resolve(String revision) throws IOException {
        String rev = revision.trim();
        if (rev.equals("HEAD")) {
            ObjectId h = head();
            if (h == null) throw new IOException("no commits yet");
            return h;
        }
        int tilde = rev.indexOf('~');
        if (tilde > 0) {
            int back = Integer.parseInt(rev.substring(tilde + 1));
            ObjectId cur = resolve(rev.substring(0, tilde));
            for (int i = 0; i < back; i++) {
                GitObject.Commit c = (GitObject.Commit) readObject(cur);
                if (c.parents().isEmpty()) {
                    throw new IOException("revision " + revision + " walks past the root commit");
                }
                cur = c.parents().get(0);
            }
            return cur;
        }
        return ObjectId.fromHex(rev);
    }

    // ------------------------------------------------------------ object IO

    /**
     * Reads an object from the loose store, falling back to the chunk pack for
     * large blobs that were stored as delta recipes.
     */
    public GitObject readObject(ObjectId id) throws IOException {
        if (loose.has(id)) {
            return loose.read(id);
        }
        // Large blobs live in the chunk pack, keyed by the same git blob id.
        if (pack.contains(id)) {
            return GitObject.blob(pack.readBlob(id));
        }
        throw new IOException("object not found: " + id.hex());
    }

    public boolean hasObject(ObjectId id) throws IOException {
        return loose.has(id) || pack.contains(id);
    }

    /** Blob content, from whichever store holds it. */
    public byte[] readBlob(ObjectId id) throws IOException {
        if (loose.has(id)) {
            return ((GitObject.Blob) loose.read(id)).content();
        }
        return pack.readBlob(id);
    }

    /**
     * Stores blob content. Small blobs become loose git objects; large ones go
     * through Rabin chunking and are delta encoded against the previous version
     * of the same path when that is cheaper.
     *
     * @return the git blob id, identical to what native git computes
     */
    public ObjectId writeBlob(byte[] content, ObjectId previousVersion) throws IOException {
        if (content.length <= LOOSE_BLOB_LIMIT) {
            return loose.write(GitObject.blob(content));
        }
        ChunkPack.StoredBlob stored = pack.store(content, previousVersion);
        return stored.id();
    }

    // ------------------------------------------------------------ staging

    private Path stageRef() {
        return refPath("stage");
    }

    private TreeBuilder loadIndex() throws IOException {
        Path p = stageRef();
        if (!Files.isRegularFile(p)) {
            return new TreeBuilder();
        }
        ObjectId treeId = ObjectId.fromHex(Files.readString(p).trim());
        return TreeBuilder.load(treeId, loose);
    }

    private void saveIndex() throws IOException {
        ObjectId treeId = index.persist(loose);
        Files.createDirectories(stageRef().getParent());
        Files.writeString(stageRef(), treeId.hex() + "\n");
    }

    /**
     * Reloads the staged tree from its stored object.
     *
     * <p>Persisting interns the whole tree, which replaces nodes with canonical
     * instances. The staging ref may be written by an older run of the tool, so
     * the tree is re-read from the object store rather than trusting whatever
     * nodes the builder happens to be holding.
     */
    private void reloadIndex() throws IOException {
        Path p = stageRef();
        if (!Files.isRegularFile(p)) {
            index = new TreeBuilder();
            return;
        }
        String hex = Files.readString(p).trim();
        if (!hex.isEmpty()) {
            index = TreeBuilder.load(ObjectId.fromHex(hex), loose);
        }
    }

    /** The staged tree. */
    public TreeBuilder index() {
        return index;
    }

    /** Stages one file from the working tree. */
    public ObjectId add(Path file) throws IOException {
        Path rel = root.relativize(file.toAbsolutePath().normalize());
        if (rel.toString().startsWith(DIRECTORY)) {
            throw new IOException("refusing to track repository internals: " + rel);
        }
        byte[] content = Files.readAllBytes(file);
        int mode = Files.isExecutable(file) ? GitObject.MODE_EXECUTABLE : GitObject.MODE_REGULAR;
        String path = rel.toString().replace('\\', '/');
        reloadIndex();
        ObjectId blobId = writeBlob(content, previousBlobId(path));
        index = index.insert(path, mode, blobId);
        dropRenamed(path, blobId);
        saveIndex();
        return blobId;
    }

    /**
     * Removes any other staged path carrying the same blob, which is what makes
     * {@code git mv} style renames behave the way a reviewer expects: moving
     * {@code b.txt} to {@code b-renamed.txt} must not leave both paths staged.
     */
    private void dropRenamed(String addedPath, ObjectId blobId) {
        for (String other : index.paths()) {
            if (other.equals(addedPath)) continue;
            TreeBuilder.Node node = index.lookup(other);
            if (node != null && node.blobId().equals(blobId)) {
                index = index.remove(other);
            }
        }
    }

    /** Stages every file under the working tree, skipping repository internals. */
    public int addAll() throws IOException {
        List<Path> files = new ArrayList<>();
        try (Stream<Path> s = Files.walk(root)) {
            s.filter(Files::isRegularFile)
                    .filter(p -> {
                        String rel = root.relativize(p).toString().replace('\\', '/');
                        return !rel.equals(DIRECTORY) && !rel.startsWith(DIRECTORY + "/")
                                && !rel.equals(".git") && !rel.startsWith(".git/");
                    })
                    .forEach(files::add);
        }
        files.sort(Comparator.comparing(Path::toString));
        reloadIndex();
        for (Path f : files) {
            Path rel = root.relativize(f);
            String path = rel.toString().replace('\\', '/');
            byte[] content = Files.readAllBytes(f);
            int mode = Files.isExecutable(f) ? GitObject.MODE_EXECUTABLE : GitObject.MODE_REGULAR;
            ObjectId blobId = writeBlob(content, previousBlobId(path));
            index = index.insert(path, mode, blobId);
        }
        saveIndex();
        return files.size();
    }

    /** Removes a path from the staged tree. */
    public void remove(String path) throws IOException {
        reloadIndex();
        index = index.remove(path);
        saveIndex();
    }

    /** The blob id currently recorded for a path in HEAD, or null. */
    public ObjectId previousBlobId(String path) throws IOException {
        ObjectId head = head();
        TreeBuilder tree = head == null ? new TreeBuilder() : TreeBuilder.load(treeOf(head), loose);
        TreeBuilder.Node node = tree.lookup(path);
        return node == null ? null : node.blobId();
    }

    /** Commits the staged tree. Returns the new commit id. */
    public ObjectId commit(String message, Identity identity) throws IOException {
        ObjectId treeId = index.persist(loose);
        ObjectId head = head();
        List<ObjectId> parents = head == null ? List.of() : List.of(head);
        GitObject.Commit commit = (GitObject.Commit) GitObject.commit(
                treeId, parents, identity.canonical(), identity.canonical(), message);
        ObjectId id = loose.write(commit);
        setHead(id);
        appendLog(id, identity, message);
        return id;
    }

    private void appendLog(ObjectId id, Identity identity, String message) throws IOException {
        String subject = message.strip().lines().findFirst().orElse("");
        String line = id.hex() + "\t" + identity.isoDate() + "\t" + identity.name() + "\t" + subject + "\n";
        Files.writeString(dir.resolve("log"), line,
                java.nio.file.StandardOpenOption.CREATE, java.nio.file.StandardOpenOption.APPEND);
    }

    /** The root tree of a commit. */
    public ObjectId treeOf(ObjectId commitId) throws IOException {
        return ((GitObject.Commit) readObject(commitId)).tree();
    }

    /** Commits reachable from HEAD, newest first. */
    public List<ObjectId> history() throws IOException {
        List<ObjectId> out = new ArrayList<>();
        ObjectId cur = head();
        while (cur != null) {
            out.add(cur);
            GitObject.Commit c = (GitObject.Commit) readObject(cur);
            cur = c.firstParent();
        }
        return out;
    }

    // ------------------------------------------------------------ checkout

    /**
     * Materialises a revision into the working tree, byte for byte.
     *
     * <p>Paths tracked by that revision are written; paths tracked by the
     * current index but absent from the revision are deleted, so checkout
     * reflects the tree exactly. Repository internals are never touched.
     */
    public int checkout(String revision) throws IOException {
        return checkout(revision, false);
    }

    public int checkout(String revision, boolean force) throws IOException {
        ObjectId commitId = resolve(revision);
        ObjectId treeId = treeOf(commitId);
        TreeBuilder target = TreeBuilder.load(treeId, loose);
        Map<String, ObjectId> want = new LinkedHashMap<>();
        for (String path : target.paths()) {
            TreeBuilder.Node node = target.lookup(path);
            // symlinks and gitlinks store their target as blob content
            want.put(path, node.blobId());
        }
        if (force) {
            index = target;
        } else {
            for (String path : index.paths()) {
                if (!want.containsKey(path)) {
                    Files.deleteIfExists(root.resolve(path));
                }
            }
            index = target;
        }
        saveIndex();
        int count = 0;
        for (Map.Entry<String, ObjectId> e : want.entrySet()) {
            Path dest = root.resolve(e.getKey());
            Files.createDirectories(dest.getParent());
            byte[] content = readBlob(e.getValue());
            Files.write(dest, content);
            count++;
        }
        return count;
    }

    /** All paths that differ between the working tree and HEAD. */
    public List<String> status() throws IOException {
        ObjectId head = head();
        TreeBuilder tree = head == null ? new TreeBuilder() : TreeBuilder.load(treeOf(head), loose);
        List<String> out = new ArrayList<>();
        for (String path : index.paths()) {
            TreeBuilder.Node node = index.lookup(path);
            TreeBuilder.Node old = tree.lookup(path);
            if (old == null) {
                out.add("A  " + path);
            } else if (!old.blobId().equals(node.blobId())) {
                out.add("M  " + path);
            }
        }
        for (String path : tree.paths()) {
            if (index.lookup(path) == null) {
                out.add("D  " + path);
            }
        }
        return out;
    }

    // -------------------------------------------------------- git interop

    /**
     * Writes an object in git's loose on-disk format so that native git can read
     * it. Used by the cross-check to prove object identity end to end.
     *
     * @return false when the object id is unknown to this repository
     */
    public boolean exportLooseObject(ObjectId id, Path gitObjectsDir) throws IOException {
        GitObject object;
        try {
            object = readObject(id);
        } catch (IOException e) {
            return false;
        }
        Path target = gitObjectsDir.resolve(id.hex().substring(0, 2)).resolve(id.hex().substring(2));
        if (Files.isRegularFile(target)) {
            return true;
        }
        Files.createDirectories(target.getParent());
        Path tmp = Files.createTempFile(target.getParent(), "tmp_obj_", null);
        try {
            try (var out = new GitObject.DeflaterStream(Files.newOutputStream(tmp))) {
                out.write(object.canonical());
            }
            Files.move(tmp, target);
        } finally {
            Files.deleteIfExists(tmp);
        }
        return true;
    }

    /**
     * Exports the full object closure of the history into a git objects
     * directory: every commit, every tree down to the leaves, and every blob.
     *
     * <p>The output is ordinary git loose object storage, so the result can be
     * handed to {@code git fsck} or dropped into a bare repository. Blobs held
     * as delta recipes are reconstructed and written in their canonical form,
     * because a native git has no way to read this store's chunk format - and it
     * does not need to, since the reconstructed bytes hash to the same id.
     *
     * @return the number of objects written
     */
    public int exportAll(Path gitObjectsDir) throws IOException {
        int n = 0;
        Deque<ObjectId> queue = new ArrayDeque<>();
        for (ObjectId commitId : history()) {
            queue.push(commitId);
        }
        Set<ObjectId> seen = new HashSet<>();
        while (!queue.isEmpty()) {
            ObjectId id = queue.pop();
            if (!seen.add(id)) continue;
            if (!exportLooseObject(id, gitObjectsDir)) continue;
            n++;
            if (loose.has(id)) {
                GitObject object = loose.read(id);
                if (object.kind() == GitObject.Kind.TREE) {
                    for (GitObject.Entry e : ((GitObject.Tree) object).entries()) {
                        queue.push(e.id);
                    }
                } else if (object.kind() == GitObject.Kind.COMMIT) {
                    GitObject.Commit c = (GitObject.Commit) object;
                    queue.push(c.tree());
                    queue.addAll(c.parents());
                }
            }
        }
        return n;
    }

    /** One line of {@code git log} style output. */
    public String logLine(int index) throws IOException {
        List<ObjectId> history = history();
        if (index < 0 || index >= history.size()) {
            throw new IOException("no such revision: " + index);
        }
        GitObject.Commit c = (GitObject.Commit) readObject(history.get(index));
        String subject = c.message().strip().lines().findFirst().orElse("");
        StringBuilder sb = new StringBuilder();
        sb.append("commit ").append(c.id().hex()).append('\n');
        for (ObjectId p : c.parents()) {
            sb.append("parent ").append(p.hex()).append('\n');
        }
        sb.append("tree   ").append(c.tree().hex()).append('\n');
        sb.append("author ").append(c.author()).append('\n');
        sb.append('\n');
        for (String line : c.message().strip().lines().toList()) {
            sb.append("    ").append(line).append('\n');
        }
        return sb.toString();
    }

    public String log(int limit) throws IOException {
        StringBuilder sb = new StringBuilder();
        int n = Math.min(limit, history().size());
        for (int i = 0; i < n; i++) {
            sb.append(logLine(i)).append('\n');
        }
        return sb.toString();
    }

    /** Human readable dump of one object, in the spirit of {@code git cat-file -p}. */
    public String catFile(String revision) throws IOException {
        ObjectId id = resolve(revision);
        GitObject object = readObject(id);
        StringBuilder sb = new StringBuilder();
        sb.append("object ").append(id.hex()).append('\n');
        sb.append("type ").append(object.kind().name().toLowerCase()).append('\n');
        sb.append("size ").append(object.size()).append('\n');
        switch (object.kind()) {
            case BLOB -> sb.append('\n').append(new String(((GitObject.Blob) object).content(),
                    StandardCharsets.UTF_8));
            case TREE -> {
                sb.append('\n');
                for (GitObject.Entry e : ((GitObject.Tree) object).entries()) {
                    sb.append(String.format("%06o %s %s%n", e.mode, e.name, e.id.hex()));
                }
            }
            case COMMIT -> {
                GitObject.Commit c = (GitObject.Commit) object;
                sb.append('\n');
                sb.append("tree ").append(c.tree().hex()).append('\n');
                for (ObjectId p : c.parents()) {
                    sb.append("parent ").append(p.hex()).append('\n');
                }
                sb.append("author ").append(c.author()).append('\n');
                sb.append("committer ").append(c.committer()).append('\n');
                sb.append('\n');
                sb.append(c.message());
            }
        }
        return sb.toString();
    }

    @Override
    public void close() {
        // Everything is written eagerly; nothing to flush.
    }

    @Override
    public String toString() {
        return "Repository[" + root + "]";
    }
}
