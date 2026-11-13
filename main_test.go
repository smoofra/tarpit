package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type entry struct {
	name     string
	typ      byte
	body     string
	linkname string
	mode     int64
}

// writeArchive builds a tar (gzipped if gz) from entries and returns its path.
func writeArchive(t *testing.T, gz bool, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
			Mode:     mode,
			ModTime:  time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	name := "a.tar"
	if gz {
		var zbuf bytes.Buffer
		zw := gzip.NewWriter(&zbuf)
		zw.Write(data)
		zw.Close()
		data = zbuf.Bytes()
		name = "a.tar.gz"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func file(name, body string) entry { return entry{name: name, typ: tar.TypeReg, body: body} }
func dir(name string) entry        { return entry{name: name, typ: tar.TypeDir, mode: 0o755} }

func mustExtract(t *testing.T, archive, dest string) *stats {
	t.Helper()
	st, err := extract(archive, dest, nil)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return st
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func TestExtract(t *testing.T) {
	for _, gz := range []bool{false, true} {
		name := "plain"
		if gz {
			name = "gzip"
		}
		t.Run(name, func(t *testing.T) {
			archive := writeArchive(t, gz,
				dir("d/"),
				file("d/one.txt", "one"),
				file("top.txt", "top"),
				file("implicit/dir/f.txt", "nested"), // no dir entry
				file("empty.txt", ""),
			)
			dest := t.TempDir()
			st := mustExtract(t, archive, dest)

			assertFile(t, filepath.Join(dest, "d", "one.txt"), "one")
			assertFile(t, filepath.Join(dest, "top.txt"), "top")
			assertFile(t, filepath.Join(dest, "implicit", "dir", "f.txt"), "nested")
			assertFile(t, filepath.Join(dest, "empty.txt"), "")
			if st.written != 4 || st.skipped != 0 || st.dirs != 1 || st.failed != 0 {
				t.Errorf("stats = %+v", *st)
			}

			fi, err := os.Stat(filepath.Join(dest, "top.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC); !fi.ModTime().Equal(want) {
				t.Errorf("mtime = %v, want %v", fi.ModTime(), want)
			}
		})
	}
}

func TestResume(t *testing.T) {
	archive := writeArchive(t, false,
		file("same.txt", "aaaa"),
		file("short.txt", "complete contents"),
		file("missing.txt", "missing"),
		file("partial.txt", "real"),
	)
	dest := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dest, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate an interrupted earlier run.
	write("same.txt", "bbbb")                     // right size: must be left alone
	write("short.txt", "comp")                    // truncated: must be rewritten
	write("partial.txt"+partialSuffix, "garbage") // stale temp file

	st := mustExtract(t, archive, dest)

	assertFile(t, filepath.Join(dest, "same.txt"), "bbbb")
	assertFile(t, filepath.Join(dest, "short.txt"), "complete contents")
	assertFile(t, filepath.Join(dest, "missing.txt"), "missing")
	assertFile(t, filepath.Join(dest, "partial.txt"), "real")
	if _, err := os.Stat(filepath.Join(dest, "partial.txt"+partialSuffix)); !os.IsNotExist(err) {
		t.Errorf("partial file still present: %v", err)
	}
	if st.written != 3 || st.skipped != 1 {
		t.Errorf("stats = %+v", *st)
	}

	// A second run has nothing left to do.
	st = mustExtract(t, archive, dest)
	if st.written != 0 || st.skipped != 4 {
		t.Errorf("rerun stats = %+v", *st)
	}
}

func TestReplacesReadOnlyFile(t *testing.T) {
	archive := writeArchive(t, false, file("ro.txt", "new contents"))
	dest := t.TempDir()
	path := filepath.Join(dest, "ro.txt")
	if err := os.WriteFile(path, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	mustExtract(t, archive, dest)
	assertFile(t, path, "new contents")
}

func TestReadOnlyArchiveModeStaysWritable(t *testing.T) {
	archive := writeArchive(t, false, entry{name: "f.txt", typ: tar.TypeReg, body: "x", mode: 0o444})
	dest := t.TempDir()
	mustExtract(t, archive, dest)
	fi, err := os.Stat(filepath.Join(dest, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("mode = %v, want owner-writable", fi.Mode())
	}
}

func TestUnsafePaths(t *testing.T) {
	archive := writeArchive(t, false,
		file("../escape.txt", "bad"),
		file("/abs.txt", "bad"),
		entry{name: "link.txt", typ: tar.TypeLink, linkname: "../outside"},
		file("ok.txt", "good"),
	)
	parent := t.TempDir()
	dest := filepath.Join(parent, "out")
	st, err := extract(archive, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("escape.txt was written outside dest")
	}
	assertFile(t, filepath.Join(dest, "ok.txt"), "good")
	if st.failed != 2 || st.written != 1 || st.hardlinks != 1 {
		t.Errorf("stats = %+v", *st)
	}
}

func TestHardLinkNotExtracted(t *testing.T) {
	archive := writeArchive(t, false,
		file("orig.txt", "shared"),
		entry{name: "sub/link.txt", typ: tar.TypeLink, linkname: "orig.txt"},
	)
	dest := t.TempDir()
	st := mustExtract(t, archive, dest)
	assertFile(t, filepath.Join(dest, "orig.txt"), "shared")
	if _, err := os.Lstat(filepath.Join(dest, "sub", "link.txt")); !os.IsNotExist(err) {
		t.Errorf("hard link was extracted: %v", err)
	}
	// Skipping hard links is intended, not a failure.
	if st.hardlinks != 1 || st.failed != 0 {
		t.Errorf("stats = %+v", *st)
	}
}

func TestSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	archive := writeArchive(t, false,
		file("target.txt", "pointed at"),
		entry{name: "sym", typ: tar.TypeSymlink, linkname: "target.txt"},
	)
	dest := t.TempDir()
	mustExtract(t, archive, dest)
	got, err := os.Readlink(filepath.Join(dest, "sym"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "target.txt" {
		t.Errorf("symlink -> %q, want target.txt", got)
	}
	// Existing symlink is left alone on rerun.
	if st := mustExtract(t, archive, dest); st.failed != 0 {
		t.Errorf("rerun stats = %+v", *st)
	}
}

func TestUnsupportedTypeIsWarning(t *testing.T) {
	archive := writeArchive(t, false,
		entry{name: "fifo", typ: tar.TypeFifo},
		file("after.txt", "still extracted"),
	)
	dest := t.TempDir()
	st := mustExtract(t, archive, dest)
	assertFile(t, filepath.Join(dest, "after.txt"), "still extracted")
	if st.failed != 1 {
		t.Errorf("stats = %+v", *st)
	}
}

func TestWriteErrorIsFatal(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs Unix permissions enforced")
	}
	archive := writeArchive(t, false, file("f.txt", "x"), file("later.txt", "y"))
	dest := t.TempDir()
	if err := os.Chmod(dest, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dest, 0o755)
	if _, err := extract(archive, dest, nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(dest, "later.txt")); !os.IsNotExist(err) {
		t.Errorf("extraction continued past fatal error")
	}
}

func TestTruncatedArchive(t *testing.T) {
	archive := writeArchive(t, false, file("big.txt", string(bytes.Repeat([]byte("z"), 10000))))
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, data[:5000], 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := extract(archive, dest, nil); err == nil {
		t.Fatal("expected error")
	}
	// Neither the final name nor the temp file should be left behind.
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Errorf("dest not empty: %v", entries)
	}
}

func TestHuman(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{5 << 20, "5.0 MiB"},
		{3 << 40, "3.0 TiB"},
	} {
		if got := human(tc.n); got != tc.want {
			t.Errorf("human(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestRefusesWritingThroughSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := t.TempDir()
	archive := writeArchive(t, false,
		entry{name: "evil", typ: tar.TypeSymlink, linkname: outside},
		file("evil/x.txt", "bad"),
		dir("evil/sub/"),
		file("evil/sub/y.txt", "bad"),
		entry{name: "d/up", typ: tar.TypeSymlink, linkname: "../../.."},
		file("d/up/z.txt", "bad"),
		file("d/ok.txt", "good"),
	)
	dest := t.TempDir()
	st, err := extract(archive, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside dest: %v", entries)
	}
	assertFile(t, filepath.Join(dest, "d", "ok.txt"), "good")
	if st.failed != 4 || st.written != 1 {
		t.Errorf("stats = %+v", *st)
	}
}

func TestRefusesSymlinkAtPartialName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := filepath.Join(t.TempDir(), "victim")
	archive := writeArchive(t, false,
		entry{name: "f.txt" + partialSuffix, typ: tar.TypeSymlink, linkname: outside},
		file("f.txt", "contents"),
	)
	dest := t.TempDir()
	mustExtract(t, archive, dest)
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("wrote through symlink at partial name: %v", err)
	}
	assertFile(t, filepath.Join(dest, "f.txt"), "contents")
}

func TestSymlinkAtCachedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := t.TempDir()
	// The fifo is unsupported, so "d" is checked and cached as a safe parent
	// but never created.  A symlink then appears at "d", and "d/y.txt" must
	// not get through on the strength of a stale cache.
	archive := writeArchive(t, false,
		entry{name: "d/fifo", typ: tar.TypeFifo},
		entry{name: "d", typ: tar.TypeSymlink, linkname: outside},
		file("d/y.txt", "bad"),
	)
	dest := t.TempDir()
	if _, err := extract(archive, dest, nil); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside dest: %v", entries)
	}
}
