package fsclient

import (
	"fmt"
	"path"
	"sort"
	"testing"
)

// BFS-019 — A DIRECTORY LISTING THROUGH THE MOUNT CAN COME BACK EMPTY (OR SHORT)
// WHILE THE DIRECTORY HAS ENTRIES.
//
// THE DEFECT, as filed: a fresh mount lists the root correctly; after ONE
// `mkdir` through the mount the same listing comes back EMPTY while every entry
// still exists on the server; after a through-mount write + unlink it comes back
// with one name while the server holds eight. `find <mount> -mindepth 1 | wc -l`
// answers 0 against the server's 136; `ls -lR` prints 2 lines against 143. rc is
// 0 throughout: it is a SILENT WRONG ANSWER, never an error. Lookups keep
// working in the same instant (`stat <mount>/go.mod` answers, `cat` answers,
// subdirectories that were never mutated list 120/120).
//
// THE PRODUCTION POINT, named: `Readdir` (internal/fsmount/fs_linux.go) answers
// from `Snapshot.Children`, and every mutator through the mount drops the
// affected directory's listing with `Snapshot.DropReaddir` so the NEXT readdir
// re-reads it. The re-read is not the problem — it happens. The problem is that
// it cannot change the answer: `Children` intersects the directory's child INDEX
// with the node set, `DropReaddir` deletes that index, and the re-read feeds the
// observed entries back through `putLocked`, which only records a parent's child
// entry when the NODE is NEW (`if !existed || old.IsDir != n.IsDir`). Every
// entry that survived the mutation is already in the node set, so the re-read
// re-inserts none of them: the index comes back holding exactly the names the
// mount created since the drop, and `Children` serves that subset — empty,
// short, or one name — with rc 0.
//
// WHY IT IS PER-DIRECTORY AND NOT A DEAD MOUNT: `Lookup` reads the node set,
// which the mutation never touches, so `stat`/`read`/`open` of a path already in
// the tree keep answering. Only the index — the thing readdir is — was emptied.
//
// THE CELLS BELOW drive that pair directly (`Put` + `DropReaddir`, the exact two
// calls every mutator makes) and then the exact shape `Readdir` drives on the
// re-read (`Put` per observed entry, then `MarkDirRead`). No kernel, no server:
// the wrong answer is produced in-process, which is what makes the mechanism
// falsifiable rather than a story about a mount.

// bfs019Entry is one node of the tree the snapshot op returns.
type bfs019Entry struct {
	path  string
	isDir bool
}

// bfs019Tree is the row's shape: six root entries, a nested collection with a
// child, and a collection with many children — so an empty answer and a SHORT
// answer are both visible.
func bfs019Tree() []bfs019Entry {
	tree := []bfs019Entry{
		{path: "", isDir: true},
		{path: "CHANGELOG.md"},
		{path: "README.md"},
		{path: "go.mod"},
		{path: "pkg", isDir: true},
		{path: "pkg/one.txt"},
		{path: "scratch", isDir: true},
		{path: "scratch/two.txt"},
		{path: "src", isDir: true},
	}
	for i := 0; i < 120; i++ {
		tree = append(tree, bfs019Entry{path: fmt.Sprintf("src/f%03d.txt", i)})
	}
	return tree
}

// bfs019Node is the Node a snapshot op / PROPFIND answer produces for an entry.
func bfs019Node(e bfs019Entry) Node {
	kind := KindFile
	if e.isDir {
		kind = KindDir
	}
	return Node{Path: e.path, IsDir: e.isDir, Mode: "0644", Kind: kind}
}

// bfs019Seed builds the tree the way SnapshotTree does: every entry Put once,
// then every collection marked read (a depth=infinity answer IS every listing).
func bfs019Seed(entries []bfs019Entry) *Snapshot {
	s := NewSnapshot("")
	for _, e := range entries {
		s.Put(bfs019Node(e))
	}
	s.MarkAllDirsRead()
	return s
}

// bfs019Names is what one READDIRPLUS answers with: the base names Children
// produces, sorted.
func bfs019Names(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, path.Base(n.Path))
	}
	sort.Strings(out)
	return out
}

// bfs019Want is the same list taken from the SERVED tree (the native control),
// so every assertion is a side-by-side and never a hardcoded string.
func bfs019Want(entries []bfs019Entry, dir string) []string {
	var out []string
	for _, e := range entries {
		if e.path == "" {
			continue
		}
		parent := path.Dir(e.path)
		if parent == "." {
			parent = ""
		}
		if parent == dir {
			out = append(out, path.Base(e.path))
		}
	}
	sort.Strings(out)
	return out
}

func bfs019Equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// bfs019Unindexed is the INVARIANT, not the symptom: every node the tree holds
// whose parent's child index does not name it. The index and the node set answer
// two different questions about one directory, so a node the tree holds and the
// index does not name is a listing that is wrong by construction.
func bfs019Unindexed(s *Snapshot) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var missing []string
	for p := range s.nodes {
		if p == "" {
			continue
		}
		parent := path.Dir(p)
		if parent == "." {
			parent = ""
		}
		if set := s.children[parent]; set != nil {
			if _, ok := set[path.Base(p)]; ok {
				continue
			}
		}
		missing = append(missing, p)
	}
	sort.Strings(missing)
	return missing
}

// bfs019Reread is the exact shape Readdir drives when the directory is not read:
// the Depth:1 answer's entries Put back, then MarkDirRead. It is the mount's own
// sequence (fs_linux.go Readdir), kept in one place here so the cells cannot
// drift from it by accident.
func bfs019Reread(s *Snapshot, dir string, entries []bfs019Entry) {
	for _, e := range entries {
		s.Put(bfs019Node(e))
	}
	s.MarkDirRead(dir)
}

// bfs019Mutation is one mutation through the mount, as the mount performs it:
// the changed node (if any), then DropReaddir of the directory whose listing
// changed — the pair fs_linux.go's Mkdir/Unlink/Rmdir/Rename/createLink and the
// write publication each perform, and the pair append.go performs.
type bfs019Mutation struct {
	name  string
	apply func(s *Snapshot, tree []bfs019Entry) []bfs019Entry
	dir   string
}

// M1 — the filed repro: one mkdir in the root.
// M2 — a write published into the root (create + publication: Drop + drop the
//
//	parent's listing).
//
// M3 — create then unlink in the root (the filed step 2).
// M4 — an unlink in a 120-entry collection.
// M5 — a rename across two directories.
// M6 — an rmdir.
// M7 — a mkdir in a NESTED collection.
func bfs019Mutations() []bfs019Mutation {
	add := func(tree []bfs019Entry, e bfs019Entry) []bfs019Entry {
		return append(append([]bfs019Entry{}, tree...), e)
	}
	del := func(tree []bfs019Entry, p string) []bfs019Entry {
		out := make([]bfs019Entry, 0, len(tree))
		for _, e := range tree {
			if e.path != p {
				out = append(out, e)
			}
		}
		return out
	}
	return []bfs019Mutation{
		{name: "mkdir in the root", dir: "", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			e := bfs019Entry{path: "oob-dir", isDir: true}
			s.Put(bfs019Node(e))
			s.DropReaddir("")
			return add(tree, e)
		}},
		{name: "write published in the root", dir: "", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			e := bfs019Entry{path: "written.txt"}
			s.Put(bfs019Node(e))
			s.Drop("written.txt") // the publication drops the path it rewrote…
			s.DropReaddir("")     // …and the parent's listing (fs_linux.go publish)
			return add(tree, e)
		}},
		{name: "create then unlink in the root", dir: "", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			e := bfs019Entry{path: "written.txt"}
			s.Put(bfs019Node(e))
			s.Drop(e.path)
			s.DropReaddir("")
			return tree
		}},
		{name: "unlink in a 120-entry collection", dir: "src", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			s.Drop("src/f000.txt")
			s.DropReaddir("src")
			return del(tree, "src/f000.txt")
		}},
		{name: "rename across two directories", dir: "", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			s.Drop("pkg/one.txt", "scratch/one.txt")
			s.DropReaddir("pkg", "scratch")
			return add(del(tree, "pkg/one.txt"), bfs019Entry{path: "scratch/one.txt"})
		}},
		{name: "rmdir", dir: "", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			s.Drop("scratch")
			s.DropReaddir("")
			return del(tree, "scratch")
		}},
		{name: "mkdir in a nested collection", dir: "pkg", apply: func(s *Snapshot, tree []bfs019Entry) []bfs019Entry {
			e := bfs019Entry{path: "pkg/inner", isDir: true}
			s.Put(bfs019Node(e))
			s.DropReaddir("pkg")
			return add(tree, e)
		}},
	}
}

// TestBFS019ReReadAfterAMutationRebuildsTheListing is the filed defect, at the
// point it is produced: the re-read happens and cannot change the answer.
func TestBFS019ReReadAfterAMutationRebuildsTheListing(t *testing.T) {
	tree := bfs019Tree()
	s := bfs019Seed(tree)

	if got, want := bfs019Names(s.Children("")), bfs019Want(tree, ""); !bfs019Equal(got, want) {
		t.Fatalf("precondition: the fresh listing is %v, the served tree holds %v", got, want)
	}

	// ONE mkdir through the mount, exactly as Mkdir performs it.
	n := Node{Path: "oob-dir", IsDir: true, Kind: KindDir}
	s.Put(n)
	s.DropReaddir("")
	if s.Known("") {
		t.Fatalf("DropReaddir left the directory marked READ: Readdir would serve the stale set and never re-read")
	}

	// THE RE-READ — the shape Readdir drives. It is not skipped; it happens.
	bfs019Reread(s, "", append(append([]bfs019Entry{}, tree...), bfs019Entry{path: "oob-dir", isDir: true}))

	if !s.Known("") {
		t.Fatalf("the re-read did not mark the directory read")
	}
	want := bfs019Want(append(append([]bfs019Entry{}, tree...), bfs019Entry{path: "oob-dir", isDir: true}), "")
	got := bfs019Names(s.Children(""))
	if !bfs019Equal(got, want) {
		t.Fatalf("BFS-019: after ONE mkdir and the re-read that follows it, readdir answers %v while the served tree holds %v — and it answers with rc 0, reporting no fault at all", got, want)
	}
	if un := bfs019Unindexed(s); len(un) != 0 {
		t.Fatalf("BFS-019: the child index does not name %v, which the tree holds: readdir cannot see them", un)
	}
}

// TestBFS019EveryMutationShapeLeavesAListableDirectory is the general case: the
// same pair, one mutation at a time, asserted against the SERVED tree's own
// listing (taken from the mutation's own effect, never a hardcoded string).
func TestBFS019EveryMutationShapeLeavesAListableDirectory(t *testing.T) {
	for _, m := range bfs019Mutations() {
		t.Run(m.name, func(t *testing.T) {
			tree := bfs019Tree()
			s := bfs019Seed(tree)
			after := m.apply(s, tree)

			// The re-read of every directory the mutation touched, plus the root
			// (a mount re-reads what the kernel asks for).
			for _, dir := range []string{"", "pkg", "scratch", "src"} {
				bfs019Reread(s, dir, after)
			}

			for _, dir := range []string{"", "pkg", "scratch", "src"} {
				got, want := bfs019Names(s.Children(dir)), bfs019Want(after, dir)
				if !bfs019Equal(got, want) {
					t.Errorf("BFS-019 [%s]: listing of %q through the mount is %v; the served tree holds %v (rc 0, no fault reported)", m.name, dir, got, want)
				}
			}
			if un := bfs019Unindexed(s); len(un) != 0 {
				t.Errorf("BFS-019 [%s]: the child index does not name %v, which the tree holds", m.name, un)
			}
		})
	}
}

// TestBFS019ARepeatedlyMutatedDirectoryStaysListable: the row's first general
// arm — it takes ONE mkdir to poison a directory, so a directory mutated three
// times must not degrade further.
func TestBFS019ARepeatedlyMutatedDirectoryStaysListable(t *testing.T) {
	tree := bfs019Tree()
	s := bfs019Seed(tree)
	for i := 1; i <= 3; i++ {
		name := "repeat-" + fmt.Sprint(i)
		s.Put(Node{Path: name, IsDir: true, Kind: KindDir})
		s.DropReaddir("")
		tree = append(tree, bfs019Entry{path: name, isDir: true})
		bfs019Reread(s, "", tree)
		if got, want := bfs019Names(s.Children("")), bfs019Want(tree, ""); !bfs019Equal(got, want) {
			t.Fatalf("BFS-019: after mkdir #%d the listing is %v; the served tree holds %v", i, got, want)
		}
	}
}

// TestBFS019ADropInvalidatesTheDirectoryItTookANameFrom is the SECOND half, and it
// is not the same defect: the mount's own mutations drop a directory's whole
// listing (`DropReaddir`, asserted above), but the INVALIDATION channel drops the
// individual names an event carries (`Drop`) — including the CHANGED COLLECTIONS
// themselves, which the surface spells `"."` and `"src"`. Removing a name from the
// index while the directory stays `Known` leaves it serving a SHORT listing: a
// name that still exists on the server, silently absent, rc 0.
func TestBFS019ADropInvalidatesTheDirectoryItTookANameFrom(t *testing.T) {
	tree := bfs019Tree()
	s := bfs019Seed(tree)
	if !s.Known("") || !s.Known("src") {
		t.Fatalf("precondition: the seeded tree has read both listings")
	}

	// The event a watched surface carries for a WRITE to an existing file
	// (measured: paths=["src/f000.txt"]).
	s.Drop("src/f000.txt")
	if s.Known("src") {
		t.Fatalf("BFS-019: a drop took a name out of src's child index and left the directory marked READ: the next readdir serves the short set (the file still exists on the server) and never re-reads")
	}
	bfs019Reread(s, "src", tree) // the re-read Readdir drives
	if got, want := bfs019Names(s.Children("src")), bfs019Want(tree, "src"); !bfs019Equal(got, want) {
		t.Fatalf("BFS-019: after the drop and the re-read src lists %d of %d names", len(got), len(want))
	}

	// The event the surface carries for a NEW name (measured: paths=[".",
	// "oob-root.txt", "src", "src/oob-src.txt"] — the changed COLLECTIONS are in
	// the list, and "." is the whole root).
	s.Drop(".", "src")
	if s.Known("") || s.Known("src") {
		t.Fatalf("BFS-019: an event naming the changed collections must un-read them, not leave them being served from a set the server has contradicted")
	}
	bfs019Reread(s, "", tree)
	bfs019Reread(s, "src", tree)
	if got, want := bfs019Names(s.Children("")), bfs019Want(tree, ""); !bfs019Equal(got, want) {
		t.Fatalf("BFS-019: after the collection-naming event and the re-read, the root lists %v; the served tree holds %v", got, want)
	}
	if un := bfs019Unindexed(s); len(un) != 0 {
		t.Fatalf("BFS-019: the child index does not name %v, which the tree holds", un)
	}
}

// TestBFS019AttributionLookupsSurviveTheMutation is the ATTRIBUTION cell: it
// must stay GREEN under the BFS-019 mutation (docs/evidence/BFS-019-arms.sh
// mutations), because the defect is a wrong LISTING and not a dead tree — the
// row's own evidence is that `stat` and `cat` keep answering while the listing
// is empty. If this cell ever goes red under that mutation the two cells are not
// independent and one mutation would blanket-red the suite.
func TestBFS019AttributionLookupsSurviveTheMutation(t *testing.T) {
	s := bfs019Seed(bfs019Tree())
	// The mutation, applied: the listing is dropped and never rebuilt.
	s.DropReaddir("")
	if s.Known("") {
		t.Fatalf("precondition: the listing must be dropped for this cell to say anything")
	}
	for _, p := range []string{"go.mod", "pkg", "pkg/one.txt", "src", "src/f000.txt", "src/f119.txt"} {
		if _, ok := s.Lookup(p); !ok {
			t.Fatalf("BFS-019 attribution: lookup of %q must still answer from the node set (the defect is the listing, not the tree)", p)
		}
		if !s.Has(p) {
			t.Fatalf("BFS-019 attribution: Has(%q) must still answer", p)
		}
	}
	if s.Count() != len(bfs019Tree()) {
		t.Fatalf("BFS-019 attribution: the node set is %d nodes, the served tree has %d — the mutation must not touch it", s.Count(), len(bfs019Tree()))
	}
}
