// tarpit extracts a tar archive and can be rerun after being interrupted.
//
// Each regular file in the archive is skipped if a file already exists at the
// destination with the same size.  Files are written to a temporary name and
// renamed into place once complete, so a file killed halfway through never
// looks finished.
//
// Usage:
//
//	tarpit [-C dir] [-v] [--body-file file] archive.tar[.gz|.bz2]
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const partialSuffix = ".tarpit-partial"

var verbose = flag.Bool("v", false, "print each file as it is written")

type stats struct {
	written, skipped, dirs, symlinks, hardlinks, failed int
	bytesWritten, bytesSkipped                          int64
}

func main() {
	dest := flag.String("C", ".", "directory to extract into")
	bodyFile := flag.String("body-file", "", "also write a TSK body file listing every entry (\"-\" for stdout)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-C dir] [-v] [--body-file file] archive.tar[.gz|.bz2]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(log.Ltime)

	var body *bodyWriter
	var bodyOut *bufio.Writer
	if *bodyFile != "" {
		out := os.Stdout
		if *bodyFile != "-" {
			f, err := os.Create(*bodyFile)
			if err != nil {
				log.Fatal(err)
			}
			defer f.Close()
			out = f
		}
		bodyOut = bufio.NewWriterSize(out, 1<<20)
		body = newBodyWriter(bodyOut)
	}

	st, err := extract(flag.Arg(0), *dest, body)
	if bodyOut != nil {
		if ferr := bodyOut.Flush(); ferr != nil && err == nil {
			err = fmt.Errorf("writing body file: %w", ferr)
		}
	}
	log.Printf("done: %d written (%s), %d skipped (%s), %d dirs, %d symlinks, %d hard links not extracted, %d failed",
		st.written, human(st.bytesWritten), st.skipped, human(st.bytesSkipped), st.dirs, st.symlinks, st.hardlinks, st.failed)
	if err != nil {
		log.Fatal(err)
	}
	if st.failed > 0 {
		os.Exit(1)
	}
}

// extract unpacks archive into dest.  If body is non-nil, every entry is also
// recorded in it, whether or not it was skipped or failed.
func extract(archive, dest string, body *bodyWriter) (*stats, error) {
	st := &stats{}
	f, err := os.Open(archive)
	if err != nil {
		return st, err
	}
	defer f.Close()

	r, err := decompressor(f)
	if err != nil {
		return st, err
	}
	tr := tar.NewReader(r)
	parents := &parentChecker{dest: dest}

	lastReport := time.Now()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return st, nil
		}
		if err != nil {
			return st, fmt.Errorf("reading archive: %w", err)
		}

		if time.Since(lastReport) > 10*time.Second {
			log.Printf("progress: %d written (%s), %d skipped (%s) — at %s",
				st.written, human(st.bytesWritten), st.skipped, human(st.bytesSkipped), hdr.Name)
			lastReport = time.Now()
		}

		if body != nil && hdr.Typeflag != tar.TypeXGlobalHeader {
			if err := body.write(hdr); err != nil {
				return st, fmt.Errorf("writing body file: %w", err)
			}
		}

		if !filepath.IsLocal(filepath.FromSlash(hdr.Name)) {
			log.Printf("refusing unsafe path %q", hdr.Name)
			st.failed++
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))

		err = parents.check(target)
		if err == nil {
			err = extractEntry(tr, hdr, target, st)
		}
		if err != nil {
			// Errors writing to disk (full disk, permissions) are fatal so we
			// don't plow through millions of entries failing each one.
			// Unsupported entry types and symlink failures are only warnings.
			var w warning
			if errors.As(err, &w) {
				log.Printf("warning: %s: %v", hdr.Name, err)
				st.failed++
				continue
			}
			return st, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
}

type warning struct{ error }

// parentChecker refuses paths whose parent directories, below dest, include
// anything but real directories.  Otherwise an archive could create a symlink
// like "evil -> /etc" and then write "evil/passwd" through it.  Missing
// directories are fine: MkdirAll will create real ones.
type parentChecker struct {
	dest string
	// lastOK caches the most recently verified directory, since archives
	// tend to list many files from the same directory in a row.  A symlink
	// can't appear at or under it without first being checked itself, which
	// moves lastOK to the symlink's parent.
	lastOK string
}

func (p *parentChecker) check(target string) error {
	dir := filepath.Dir(target)
	if dir == p.lastOK {
		return nil
	}
	rel, err := filepath.Rel(p.dest, dir)
	if err != nil {
		return err
	}
	if rel != "." {
		cur := p.dest
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			fi, err := os.Lstat(cur)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if fi.Mode().Type() != fs.ModeDir {
				return warning{fmt.Errorf("refusing to write through %s: not a directory", cur)}
			}
		}
	}
	p.lastOK = dir
	return nil
}

func extractEntry(tr *tar.Reader, hdr *tar.Header, target string, st *stats) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		st.dirs++
		return os.MkdirAll(target, 0o755)

	case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
		if fi, err := os.Lstat(target); err == nil {
			if fi.Mode().IsRegular() && fi.Size() == hdr.Size {
				st.skipped++
				st.bytesSkipped += hdr.Size
				return nil
			}
			if fi.IsDir() {
				return warning{errors.New("a directory already exists at this path")}
			}
		}
		if err := writeFile(tr, hdr, target); err != nil {
			return err
		}
		if *verbose {
			log.Printf("wrote %s (%s)", hdr.Name, human(hdr.Size))
		}
		st.written++
		st.bytesWritten += hdr.Size
		return nil

	case tar.TypeSymlink:
		st.symlinks++
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return warning{err}
		}
		return nil

	case tar.TypeLink:
		// Hard links are deliberately not extracted.  Linking or copying
		// from another path in the destination can be steered by symlinks
		// in the archive to files outside it.  They still appear in the
		// body file.
		st.hardlinks++
		if *verbose {
			log.Printf("not extracting hard link %s -> %s", hdr.Name, hdr.Linkname)
		}
		return nil

	case tar.TypeXGlobalHeader:
		return nil

	default:
		return warning{fmt.Errorf("skipping unsupported entry type %q", hdr.Typeflag)}
	}
}

// writeFile copies r to a temporary file next to target, then renames it into
// place, so target only ever exists with its complete contents.
func writeFile(r io.Reader, hdr *tar.Header, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + partialSuffix
	// Remove a stale partial file first and create exclusively, so that a
	// symlink at the temporary name is never followed.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != hdr.Size {
		err = fmt.Errorf("wrote %d bytes, expected %d", n, hdr.Size)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}

	_ = os.Chtimes(tmp, hdr.ModTime, hdr.ModTime)

	// Rename can't replace a read-only file on Windows, so clear the way.
	if _, err := os.Lstat(target); err == nil {
		_ = os.Chmod(target, 0o644)
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	// Restore the archived permissions, but always keep the owner write bit
	// so a rerun can replace the file.  (On Windows, the write bit is the
	// only permission that matters.)
	_ = os.Chmod(target, fs.FileMode(hdr.Mode).Perm()|0o200)
	return nil
}

// decompressor sniffs the archive's magic bytes and wraps it in gzip or bzip2
// as needed.  Plain tars are returned unwrapped so tar.Reader can seek past
// the data of skipped files instead of reading it.
func decompressor(f *os.File) (io.Reader, error) {
	magic := make([]byte, 3)
	if _, err := f.ReadAt(magic, 0); err != nil && err != io.EOF {
		return nil, err
	}
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		return gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	case bytes.Equal(magic, []byte("BZh")):
		return bzip2.NewReader(bufio.NewReaderSize(f, 1<<20)), nil
	default:
		return f, nil
	}
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
