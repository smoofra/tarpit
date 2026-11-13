# tarpit

Extract a tar archive in a way that can be resumed after being interrupted.

If a long extraction gets killed partway through, run tarpit again with the
same arguments.  Files that already exist at the destination with the right
size are skipped, and it picks up where it left off.

## Install

```
go install github.com/smoofra/tarpit@latest
```

This puts `tarpit` in `$(go env GOPATH)/bin`.

To build a Windows binary from another OS:

```
GOOS=windows GOARCH=amd64 go install github.com/smoofra/tarpit@latest
```

which puts `tarpit.exe` in `$(go env GOPATH)/bin/windows_amd64`.

## Usage

```
tarpit [-C dir] [-v] [--body-file file] archive.tar[.gz|.bz2]
```

| Flag | Meaning |
| --- | --- |
| `-C dir` | Directory to extract into (default `.`) |
| `-v` | Print each file as it is written |
| `--body-file file` | Also write a [TSK body file](https://wiki.sleuthkit.org/index.php?title=Body_file) listing every entry in the archive (`-` for stdout) |

Gzip and bzip2 archives are detected automatically.

## How resuming works

- A regular file is skipped if a file already exists at its destination with
  the same size.  Contents are not compared.
- Files are written to `name.tarpit-partial` and renamed into place once
  complete, so a file interrupted halfway never has its final name.
- With an uncompressed `.tar`, skipped files are seeked past rather than read,
  so catching up to where a previous run stopped is fast.  Compressed archives
  still have to be decompressed from the start.

tarpit exits 0 on success and 1 if anything failed, including entries it only
warned about (such as symlinks Windows wouldn't create), so check the log
before rerunning in a loop.

## Body file

`--body-file` writes one line per archive entry in the Sleuth Kit 3.x body file
format, suitable for `mactime`.  Every entry is listed, including ones skipped
because they were already extracted, so a resumed run still produces a complete
body file.

- MD5 and inode are always `0`.
- atime and ctime are filled in only when the archive records them (PAX or GNU
  format); crtime is always `0`.
- Hard links report the size of the file they link to.
