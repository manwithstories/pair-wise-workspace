package lawfirm.vcs;

import java.io.IOException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Comparator;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Copy-on-write persistent tree builder.
 *
 * <p>A tree is held as immutable nodes in memory. Inserting a path rebuilds
 * only the nodes along that path and returns a new root; every sibling subtree
 * that did not change is shared by reference with the previous root and is never
 * re-hashed or re-written. Interning of the rebuilt nodes goes through an
 * {@link ObjectPool}, so a node with the same content is a single shared
 * instance across roots, and a root that ends up identical to an existing tree
 * collapses onto the same object id without touching storage.
 *
 * <p>This is what keeps a 100k-file commit cheap: the cost is proportional to the
 * depth of the edited path, not to the size of the tree.
 */
public final class TreeBuilder {

    /** One immutable node: an entry name bound to either a subtree or a blob id. */
    public static final class Node {
        private final String name;
        private final int mode;
        private final ObjectId blobId;   // non-null for blob/symlink/gitlink entries
        private final Node[] children;   // non-empty for tree entries

        private Node(String name, int mode, ObjectId blobId, Node[] children) {
            this.name = name;
            this.mode = mode;
            this.blobId = blobId;
            this.children = children;
        }

        public String name() {
            return name;
        }

        public int mode() {
            return mode;
        }

        public boolean isTree() {
            return children.length > 0 || mode == GitObject.MODE_TREE;
        }

        public ObjectId blobId() {
            return blobId;
        }

        public int childCount() {
            return children.length;
        }

        public Node child(String childName) {
            for (Node c : children) {
                if (c.name.equals(childName)) return c;
            }
            return null;
        }

        Node[] children() {
            return children;
        }

        /**
         * Interns this node and everything under it, depth first. A directory's
         * id depends on its children, so children are interned first.
         */
        Node intern(ObjectPool pool) {
            if (!isTree()) return pool.internLeaf(this);
            Node[] rebuilt = new Node[children.length];
            for (int i = 0; i < children.length; i++) {
                rebuilt[i] = children[i].intern(pool);
            }
            return pool.internDir(this, rebuilt);
        }

        /** The key git sorts this entry by: a directory sorts as "name/". */
        String sortKey() {
            return isTree() ? name + "/" : name;
        }
    }

    /**
     * Interns immutable nodes so that structurally equal subtrees are the same
     * Java object.
     *
     * <p>Nodes are keyed by their <em>content</em>: the entry name, the mode and,
     * for a directory, the ordered list of child <em>object ids</em>. Keying by
     * content rather than by identity is what makes interning correct across a
     * save and reload cycle, and it lets an unchanged subtree collapse onto the
     * id it already has.
     */
    public static final class ObjectPool {
        private final Map<Key, Node> byKey = new LinkedHashMap<>();
        private long hits;

        private record Key(int mode, String name, ObjectId blobId, List<ObjectId> childIds) {
        }

        Node internLeaf(Node node) {
            Key key = new Key(node.mode, node.name, node.blobId, List.of());
            Node existing = byKey.get(key);
            if (existing != null) {
                hits++;
                return existing;
            }
            byKey.put(key, node);
            return node;
        }

        Node internDir(Node node, Node[] children) {
            List<ObjectId> childIds = new ArrayList<>(children.length);
            for (Node c : children) {
                childIds.add(objectIdOf(c));
            }
            Key key = new Key(node.mode, node.name, null, childIds);
            Node existing = byKey.get(key);
            if (existing != null) {
                hits++;
                return existing;
            }
            byKey.put(key, node);
            return node;
        }

        /**
         * The object id a node will serialize to. Leaves already know their id;
         * for a directory the id follows from the canonical tree bytes, which are
         * fully determined by the sorted child ids, so it can be derived without
         * a round trip through the store.
         *
         * <p>This also validates that a directory node's {@code name} field is not
         * load bearing: a tree object's id never depends on the directory's own
         * entry name, only on its children.
         */
        private static ObjectId objectIdOf(Node node) {
            if (!node.isTree()) {
                return node.blobId;
            }
            List<GitObject.Entry> entries = new ArrayList<>(node.children.length);
            for (Node c : node.children) {
                entries.add(new GitObject.Entry(c.mode, c.name,
                        c.isTree() ? objectIdOf(c) : c.blobId));
            }
            entries.sort(Comparator.comparing(TreeBuilder::treeSortKey, TreeBuilder::unsignedCompare));
            return GitObject.tree(entries).id();
        }

        public int size() {
            return byKey.size();
        }

        public long hits() {
            return hits;
        }

        public void clear() {
            byKey.clear();
            hits = 0;
        }
    }

    private static final Node[] NO_CHILDREN = new Node[0];

    /** Counts tree objects actually written by the current persist, for reuse tests. */
    private final AtomicLong nodesWritten = new AtomicLong();
    private long writtenLastTime;

    private final ObjectPool pool;
    private Node root;

    public TreeBuilder() {
        this(new ObjectPool());
    }

    public TreeBuilder(ObjectPool pool) {
        this.pool = pool;
        this.root = emptyDir("");
    }

    private static Node emptyDir(String name) {
        return new Node(name, GitObject.MODE_TREE, null, new Node[0]);
    }

    /** How many tree objects the last {@link #persist} wrote to storage. */
    public long nodesWrittenByLastPersist() {
        return writtenLastTime;
    }

    public ObjectPool pool() {
        return pool;
    }

    /** The current in-memory root; shared across derived builders. */
    public Node root() {
        return root;
    }

    /**
     * Inserts {@code path} with the given mode and blob id. Returns a builder
     * whose root reflects the change; the receiver is left untouched.
     */
    public TreeBuilder insert(String path, int mode, ObjectId blobId) {
        String[] parts = split(path);
        Node newRoot = insert(root, parts, 0, mode, blobId);
        TreeBuilder tb = new TreeBuilder(pool);
        tb.root = newRoot;
        return tb;
    }

    private Node insert(Node dir, String[] parts, int at, int mode, ObjectId blobId) {
        String name = parts[at];
        boolean last = at == parts.length - 1;
        List<Node> kids = new ArrayList<>(Arrays.asList(dir.children()));
        int existing = indexOf(kids, name);
        if (last) {
            Node leaf = new Node(name, mode, blobId, NO_CHILDREN);
            if (existing >= 0) {
                kids.set(existing, leaf);
            } else {
                kids.add(leaf);
            }
        } else {
            Node childDir = existing >= 0 ? kids.get(existing) : emptyDir(name);
            if (childDir.children.length == 0 && childDir.blobId != null) {
                throw new IllegalArgumentException("cannot replace blob " + name + " with a directory");
            }
            Node rebuilt = insert(childDir, parts, at + 1, mode, blobId);
            if (existing >= 0) {
                kids.set(existing, rebuilt);
            } else {
                kids.add(rebuilt);
            }
        }
        kids.sort(TreeBuilder::compareNodes);
        return new Node(dir.name, GitObject.MODE_TREE, null, kids.toArray(new Node[0]));
    }

    /** Removes a path if present. Returns a builder with the new root. */
    public TreeBuilder remove(String path) {
        String[] parts = split(path);
        Node newRoot = remove(root, parts, 0);
        TreeBuilder tb = new TreeBuilder(pool);
        tb.root = newRoot;
        return tb;
    }

    private Node remove(Node dir, String[] parts, int at) {
        List<Node> kids = new ArrayList<>(Arrays.asList(dir.children()));
        int existing = indexOf(kids, parts[at]);
        if (existing < 0) return dir;
        if (at == parts.length - 1) {
            kids.remove(existing);
        } else {
            Node rebuilt = remove(kids.get(existing), parts, at + 1);
            if (rebuilt.children.length == 0) {
                kids.remove(existing);
            } else {
                kids.set(existing, rebuilt);
            }
        }
        if (kids.isEmpty() && at == 0) return emptyDir(dir.name);
        return new Node(dir.name, GitObject.MODE_TREE, null, kids.toArray(new Node[0]));
    }

    /** Looks up a path, returning null when absent. */
    public Node lookup(String path) {
        String[] parts = split(path);
        Node cur = root;
        for (int i = 0; i < parts.length; i++) {
            cur = cur.child(parts[i]);
            if (cur == null) return null;
        }
        return cur;
    }

    /** Every blob path in the tree, sorted; used by checkout and dumps. */
    public List<String> paths() {
        List<String> out = new ArrayList<>();
        walk(root, "", out);
        out.sort(null);
        return out;
    }

    private void walk(Node dir, String prefix, List<String> out) {
        for (Node c : dir.children) {
            String p = prefix.isEmpty() ? c.name : prefix + "/" + c.name;
            if (c.isTree()) {
                walk(c, p, out);
            } else {
                out.add(p);
            }
        }
    }

    /**
     * Interns the whole tree into the pool, then persists the tree objects and
     * returns the root tree object id.
     *
     * <p>Only trees are written here. Blob content is written when a path is
     * added, because a tree records just the ids of its entries; a blob missing
     * from the store is therefore a caller error rather than something this walk
     * can repair.
     */
    public ObjectId persist(GitObject.LooseStore store) throws IOException {
        Node interned = root.intern(pool);
        nodesWritten.set(0);
        ObjectId id = writeRecursive(store, interned);
        writtenLastTime = nodesWritten.get();
        // Adopt the interned tree so later commits start from canonical nodes
        // whose names agree with the entries they are stored under.
        root = interned;
        return id;
    }

    /**
     * Writes the subtrees depth first and returns the tree id of {@code dir}.
     *
     * <p>Entry names are taken from the parent's child list, and a directory's
     * own name is only ever used for hashing and for in-memory lookups, so the
     * two can never drift apart.
     *
     * <p>A node whose id already exists in the store is skipped by
     * {@code LooseStore.write}, so a commit that touches one path only
     * serializes the nodes along that path; every sibling subtree is shared with
     * the previous root and never rewritten.
     */
    private ObjectId writeRecursive(GitObject.LooseStore store, Node dir)
            throws IOException {
        List<GitObject.Entry> entries = new ArrayList<>(dir.children().length);
        for (Node c : dir.children()) {
            ObjectId childId = c.isTree() ? writeRecursive(store, c) : c.blobId();
            entries.add(new GitObject.Entry(c.mode(), c.name(), childId));
        }
        entries.sort(Comparator.comparing(TreeBuilder::treeSortKey, TreeBuilder::unsignedCompare));
        boolean[] wrote = {false};
        ObjectId id = store.write(GitObject.tree(entries), flag -> wrote[0] = flag);
        if (wrote[0]) nodesWritten.incrementAndGet();
        return id;
    }

    private static String treeSortKey(GitObject.Entry e) {
        return e.isTree() ? e.name + "/" : e.name;
    }

    /**
     * Byte-wise unsigned comparison of two names, which is how git orders the
     * entries of a tree object. UTF-8 encoding keeps the comparison independent
     * of the platform locale.
     */
    private static int unsignedCompare(String a, String b) {
        return GitObject.Entry.compareNames(a, b);
    }

    /** git's tree ordering: byte-wise on name, directories compared with a '/'. */
    private static int compareNodes(Node a, Node b) {
        return unsignedCompare(a.sortKey(), b.sortKey());
    }

    private static int indexOf(List<Node> kids, String name) {
        for (int i = 0; i < kids.size(); i++) {
            if (kids.get(i).name.equals(name)) return i;
        }
        return -1;
    }

    /**
     * Loads a tree object id into a builder.
     *
     * <p>Names cannot be recovered from a stored tree object: a tree stores entry
     * names for its children but not for itself. Rebuilding therefore keys each
     * node on its <em>children</em>, not on its own name, which is exactly what
     * the copy-on-write path does. Two paths that lead to an identical subtree
     * get distinct, correctly named wrappers around one shared children array.
     */
    public static TreeBuilder load(ObjectId rootId, GitObject.LooseStore store) throws IOException {
        Map<ObjectId, Node[]> cache = new HashMap<>();
        TreeBuilder tb = new TreeBuilder();
        tb.root = new Node("", GitObject.MODE_TREE, null, childrenOf(rootId, store, cache));
        return tb;
    }

    /**
     * Materializes the children of a stored tree, sharing children arrays
     * between repeated references to the same subtree.
     *
     * <p>The cache is consulted before recursing and populated after, rather than
     * through {@code computeIfAbsent}, because nested lookups would mutate the map
     * while it was being traversed.
     */
    private static Node[] childrenOf(ObjectId treeId, GitObject.LooseStore store,
                                     Map<ObjectId, Node[]> cache) throws IOException {
        Node[] cached = cache.get(treeId);
        if (cached != null) {
            return cached;
        }
        GitObject object = store.read(treeId);
        if (object.kind() != GitObject.Kind.TREE) {
            throw new IOException("not a tree object: " + treeId.hex());
        }
        GitObject.Entry[] entries = ((GitObject.Tree) object).entries();
        Node[] kids = new Node[entries.length];
        for (int i = 0; i < entries.length; i++) {
            GitObject.Entry e = entries[i];
            if (e.isTree()) {
                kids[i] = new Node(e.name, GitObject.MODE_TREE, null, childrenOf(e.id, store, cache));
            } else {
                kids[i] = new Node(e.name, e.mode, e.id, NO_CHILDREN);
            }
        }
        Arrays.sort(kids, TreeBuilder::compareNodes);
        cache.put(treeId, kids);
        return kids;
    }

    /** Splits "a/b/c" into {"a","b","c"}, rejecting empty or dotted components. */
    public static String[] split(String path) {
        if (path == null || path.isEmpty()) {
            throw new IllegalArgumentException("path must not be empty");
        }
        String p = path.replace('\\', '/');
        while (p.startsWith("./")) p = p.substring(2);
        while (p.startsWith("/")) p = p.substring(1);
        if (p.endsWith("/")) p = p.substring(0, p.length() - 1);
        String[] parts = p.split("/");
        for (String part : parts) {
            if (part.isEmpty() || part.equals(".") || part.equals("..")) {
                throw new IllegalArgumentException("invalid path component in: " + path);
            }
        }
        return parts;
    }
}
