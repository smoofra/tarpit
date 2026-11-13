package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	t1 = time.Unix(1600000000, 0)
	t2 = time.Unix(1600000100, 0)
	t3 = time.Unix(1600000200, 0)
)

// writeHeaders writes a tar of raw headers, so tests can set fields like
// uid/gid and access times that writeArchive doesn't.
func writeHeaders(t *testing.T, hdrs ...*tar.Header) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func extractBody(t *testing.T, archive, dest string) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := extract(archive, dest, newBodyWriter(&buf)); err != nil {
		t.Fatalf("extract: %v", err)
	}
	return buf.String()
}

func TestBodyFile(t *testing.T) {
	archive := writeHeaders(t,
		&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 100, ModTime: t1},
		&tar.Header{Name: "dir/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 42, Uid: 1000, Gid: 100,
			ModTime: t1, AccessTime: t2, ChangeTime: t3, Format: tar.FormatPAX},
		&tar.Header{Name: "dir/sym", Typeflag: tar.TypeSymlink, Linkname: "file.txt", Mode: 0o777, ModTime: t2},
		&tar.Header{Name: "dir/hard.txt", Typeflag: tar.TypeLink, Linkname: "dir/file.txt", Mode: 0o644, ModTime: t1},
		&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o600, ModTime: t3},
		&tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3, ModTime: t1},
	)

	got := extractBody(t, archive, t.TempDir())
	want := strings.Join([]string{
		"0|dir/|0|d/drwxr-xr-x|1000|100|0|0|1600000000|0|0",
		"0|dir/file.txt|0|r/rrw-r--r--|1000|100|42|1600000100|1600000000|1600000200|0",
		"0|dir/sym -> file.txt|0|l/lrwxrwxrwx|0|0|0|0|1600000100|0|0",
		"0|dir/hard.txt|0|r/rrw-r--r--|0|0|42|0|1600000000|0|0",
		"0|fifo|0|p/prw-------|0|0|0|0|1600000200|0|0",
		// Refused entries are still listed: the body file describes the archive.
		"0|../evil.txt|0|r/rrw-r--r--|0|0|3|0|1600000000|0|0",
		"",
	}, "\n")
	if got != want {
		t.Errorf("body file:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBodyFileIncludesSkipped(t *testing.T) {
	archive := writeArchive(t, false, dir("d/"), file("d/a.txt", "aaa"), file("b.txt", "bb"))
	dest := t.TempDir()
	first := extractBody(t, archive, dest)

	var buf bytes.Buffer
	st, err := extract(archive, dest, newBodyWriter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if st.skipped != 2 || st.written != 0 {
		t.Fatalf("second run should skip everything, stats = %+v", *st)
	}
	if buf.String() != first {
		t.Errorf("resumed body file differs:\n%s\nvs first run:\n%s", buf.String(), first)
	}
	if n := strings.Count(first, "\n"); n != 3 {
		t.Errorf("got %d lines, want 3:\n%s", n, first)
	}
}

func TestBodyFileGzip(t *testing.T) {
	archive := writeArchive(t, true, file("a.txt", "hello"))
	got := extractBody(t, archive, t.TempDir())
	want := "0|a.txt|0|r/rrw-r--r--|0|0|5|0|1577934245|0|0\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBodyFileGlobalHeaderOmitted(t *testing.T) {
	archive := writeHeaders(t,
		&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "hi"}, Format: tar.FormatPAX},
		&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, ModTime: t1},
	)
	got := extractBody(t, archive, t.TempDir())
	if strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "0|a.txt|") {
		t.Errorf("got %q", got)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestBodyFileWriteErrorIsFatal(t *testing.T) {
	archive := writeArchive(t, false, file("a.txt", "a"))
	if _, err := extract(archive, t.TempDir(), newBodyWriter(failWriter{})); err == nil {
		t.Fatal("expected error")
	}
}

func TestModeString(t *testing.T) {
	for _, tc := range []struct {
		typ  byte
		mode int64
		want string
	}{
		{tar.TypeReg, 0o644, "r/rrw-r--r--"},
		{tar.TypeReg, 0o4755, "r/rrwsr-xr-x"},
		{tar.TypeReg, 0o4644, "r/rrwSr--r--"},
		{tar.TypeReg, 0o2755, "r/rrwxr-sr-x"},
		{tar.TypeDir, 0o1777, "d/drwxrwxrwt"},
		{tar.TypeDir, 0o1776, "d/drwxrwxrwT"},
		{tar.TypeChar, 0o666, "c/crw-rw-rw-"},
		{tar.TypeBlock, 0o660, "b/brw-rw----"},
		{tar.TypeCont, 0o644, "-/-rw-r--r--"},
	} {
		if got := modeString(&tar.Header{Typeflag: tc.typ, Mode: tc.mode}); got != tc.want {
			t.Errorf("modeString(%q, %o) = %q, want %q", tc.typ, tc.mode, got, tc.want)
		}
	}
}

type closeFailer struct {
	bytes.Buffer
	closed bool
}

func (c *closeFailer) Close() error {
	c.closed = true
	return errors.New("close failed")
}

func TestBodyFileCloseErrorReported(t *testing.T) {
	archive := writeArchive(t, false, file("a.txt", "a"))
	out := &closeFailer{}
	bf := newBodyFile(out)
	if _, err := extract(archive, t.TempDir(), bf.bodyWriter); err != nil {
		t.Fatal(err)
	}
	if err := bf.Close(); err == nil {
		t.Error("Close succeeded despite the underlying close failing")
	}
	if !out.closed {
		t.Error("underlying file was not closed")
	}
	if !strings.HasPrefix(out.String(), "0|a.txt|") {
		t.Errorf("buffered data was not flushed: %q", out.String())
	}
}

func TestOpenBodyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.body")
	bf, err := openBodyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := writeArchive(t, false, file("a.txt", "a"))
	if _, err := extract(archive, t.TempDir(), bf.bodyWriter); err != nil {
		t.Fatal(err)
	}
	if err := bf.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "0|a.txt|") {
		t.Errorf("body file = %q", got)
	}
}
