package main

import (
	"archive/tar"
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"time"
)

// bodyFile is a buffered bodyWriter on a file that must be closed, so that
// errors from flushing and closing it are reported.
type bodyFile struct {
	*bodyWriter
	buf *bufio.Writer
	c   io.Closer
}

// openBodyFile creates the body file at path, or uses stdout if path is "-".
func openBodyFile(path string) (*bodyFile, error) {
	if path == "-" {
		return newBodyFile(nopCloser{os.Stdout}), nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return newBodyFile(f), nil
}

func newBodyFile(wc io.WriteCloser) *bodyFile {
	buf := bufio.NewWriterSize(wc, 1<<20)
	return &bodyFile{bodyWriter: newBodyWriter(buf), buf: buf, c: wc}
}

func (b *bodyFile) Close() error {
	err := b.buf.Flush()
	if cerr := b.c.Close(); err == nil {
		err = cerr
	}
	return err
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// bodyWriter writes one line per archive entry in the Sleuth Kit 3.x body
// file format, for timelining with mactime:
//
//	MD5|name|inode|mode_as_string|UID|GID|size|atime|mtime|ctime|crtime
//
// MD5 and inode are always 0: hashing would mean reading the data of files
// we skip, and tar has no inodes.  Times tar doesn't record are 0.
type bodyWriter struct {
	w io.Writer
	// sizes remembers regular file sizes so hard links, which have no size of
	// their own in tar, can report the size of the file they point to.
	sizes map[string]int64
}

func newBodyWriter(w io.Writer) *bodyWriter {
	return &bodyWriter{w: w, sizes: make(map[string]int64)}
}

func (b *bodyWriter) write(hdr *tar.Header) error {
	size := hdr.Size
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
		b.sizes[path.Clean(hdr.Name)] = hdr.Size
	case tar.TypeLink:
		size = b.sizes[path.Clean(hdr.Linkname)]
	case tar.TypeDir, tar.TypeSymlink:
		size = 0
	}

	name := hdr.Name
	if hdr.Typeflag == tar.TypeSymlink {
		name += " -> " + hdr.Linkname
	}

	_, err := fmt.Fprintf(b.w, "0|%s|0|%s|%d|%d|%d|%d|%d|%d|0\n",
		name, modeString(hdr), hdr.Uid, hdr.Gid, size,
		epoch(hdr.AccessTime), epoch(hdr.ModTime), epoch(hdr.ChangeTime))
	return err
}

func epoch(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// modeString renders a mode the way TSK does, e.g. "r/rrwxr-xr-x": the type
// from the directory entry, a slash, then the type and permissions from the
// metadata.  Tar only has one type, so it's repeated.
func modeString(hdr *tar.Header) string {
	var t byte
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse, tar.TypeLink:
		t = 'r'
	case tar.TypeDir:
		t = 'd'
	case tar.TypeSymlink:
		t = 'l'
	case tar.TypeChar:
		t = 'c'
	case tar.TypeBlock:
		t = 'b'
	case tar.TypeFifo:
		t = 'p'
	default:
		t = '-'
	}

	m := hdr.Mode
	perm := []byte("rwxrwxrwx")
	for i := range perm {
		if m&(1<<(8-i)) == 0 {
			perm[i] = '-'
		}
	}
	special := func(bit int64, i int, set, unset byte) {
		if m&bit != 0 {
			if perm[i] == 'x' {
				perm[i] = set
			} else {
				perm[i] = unset
			}
		}
	}
	special(04000, 2, 's', 'S') // setuid
	special(02000, 5, 's', 'S') // setgid
	special(01000, 8, 't', 'T') // sticky

	return string([]byte{t, '/', t}) + string(perm)
}
