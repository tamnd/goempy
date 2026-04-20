# goempy

`goempy` bakes a CPython 3.14 interpreter into your Go binary. You call
`python.NewEmbeddedPython("myapp")`, you get back an `*exec.Cmd` factory,
and you run Python code on hosts that have no Python installed. There is no
CGo, no libpython to hunt for, no PyInstaller trick.

This is a fork of [`kluctl/go-embed-python`](https://github.com/kluctl/go-embed-python).
I started the fork in April 2026 after Python 3.14 shipped and upstream had
been quiet for a few months. The design, the build pipeline, the pip
integration, the runtime extractor: all of that is the original authors'
work. See [Credits](#credits) for the full attribution.

```go
package main

import (
	"os"

	"github.com/tamnd/goempy/python"
)

func main() {
	ep, err := python.NewEmbeddedPython("hello")
	if err != nil {
		panic(err)
	}
	defer ep.Cleanup()

	cmd, _ := ep.PythonCmd("-c", "import sys; print(sys.version)")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
```

Build that and run it. On the first invocation you get roughly 22 MB of
Python extracted into `$TMPDIR/go-embedded-python-hello-<hash>/`. The
binary then execs `bin/python3 -c …` as a subprocess. Subsequent
invocations reuse the same directory. The integrity check compares each
file's size on disk against the embedded manifest and skips writing when
the two agree.

## Why bother

There are only a handful of ways to run Python from Go, and most of them
are annoying.

The CGo route with [`go-python/cpy3`](https://github.com/go-python/cpy3)
and friends works, but you own the problem of getting the correct
libpython onto every machine you ship to. Cross-compilation is painful.
Long-running processes tend to pick up edge cases around the GIL and
reference counting.

A sidecar Python install works, but then you are shipping an installer
for each platform, and you cannot cleanly vendor your Python dependencies
with `go get`.

[PyOxidizer](https://github.com/indygreg/PyOxidizer) and
[pyembed](https://github.com/indygreg/PyOxidizer/tree/main/pyembed) embed
CPython in-process, Rust-side, and the project is effectively parked.

The approach in `kluctl/go-embed-python`, and now this fork, is to put
the entire CPython tree into the Go binary via `//go:embed`, extract it
on first run, and exec it as a subprocess. That trades about 25 to 30 MB
of compressed binary size for a deployment story that is just `go build`.
For CLI tools, Kubernetes operators, and GitOps controllers that want
templating or a pure-Python library or two, I have not found a cleaner
option.

## Architecture

There are three times that matter: release time (when CI builds the
tagged artifact), build time (when a user compiles their app against a
tag), and runtime (when that app runs).

### Release time

`python/generate/main.go` runs once per `(python, pbs, platform)` tuple.
For each tuple it does the following.

It downloads the matching
`cpython-<version>+<pbs>-<triple>-pgo+lto-full.tar.zst` asset from
[python-build-standalone](https://github.com/astral-sh/python-build-standalone).
Windows uses `pgo-full` (no LTO on MSVC builds).

It decompresses with `klauspost/compress/zstd`, streams through
`archive/tar`, and writes the result into a staging directory.

It removes parts of the stdlib we do not want to carry. Right now the
removal list is `ensurepip`, `idlelib`, `lib2to3`, `pydoc_data`,
`site-packages`, `test`, `turtledemo`, and the stray `bin` directory
that some packages install into.

It runs `internal.CleanupPythonDir` to apply a keep-glob pattern. On
Unix the keep list is `bin/**`, `lib/*.so*`, `lib/*.dylib`, and
`lib/python3.*/**`. On Windows it is `Lib/**`, `DLLs/**`, `*.dll`, and
`*.exe`. Everything else in the install tree gets deleted.

It calls `embed_util.CopyForEmbed`, which walks the cleaned tree,
gzip-compresses every regular file at `BestCompression`, preserves
symlinks in a manifest, and writes the result to
`python/internal/data/<goos>-<goarch>/`. The manifest lands in
`files.json` alongside the compressed payload and is content-hashed.

Finally it emits a small `embed_<goos>_<goarch>.go` stub with a build
constraint and a `//go:embed all:<goos>-<goarch>` directive so the Go
compiler picks up only the right platform's bytes.

`pip/generate` does roughly the same thing for pip-installed wheels.
It drives the freshly-extracted interpreter through `pip install -r
requirements.txt --platform <tag> --only-binary=:all:`, then packs
each target directory using the same `embed_util.CopyForEmbed` helper.

The release workflow finishes by committing
`python/internal/data/` and `pip/internal/data/` to a detached branch
and tagging it. The primary Python line (currently 3.14) gets a clean
`v<lib>` tag, so `go get @latest` resolves to it. Secondary lines get
a semver prerelease suffix like `v<lib>-py3.13.13`. The main branch
stays slim because the binary data never lives on it.

### Build time

In a user's application you write `import "github.com/tamnd/goempy/python"`
and pin a specific release tag. The Go compiler resolves that tag,
pulls in the tree with the committed `python/internal/data/`, and
evaluates the per-platform `embed.go` files. Each file looks like this:

```go
//go:build linux && amd64

package data

import (
	"embed"
	"io/fs"
)

//go:embed all:linux-amd64
var _data embed.FS
var Data, _ = fs.Sub(_data, "linux-amd64")
```

The build constraints mean the linker only embeds the bytes for the
`GOOS`/`GOARCH` combo you are compiling for. A `GOOS=linux GOARCH=arm64`
build pulls in one `linux-arm64` tree and nothing else.

### Runtime

`python.NewEmbeddedPython(name)` is the only entry point most users
need. Under the hood:

```
NewEmbeddedPython(name)
  └─ embed_util.NewEmbeddedFiles(data.Data, "python-"+name)
        ├─ read files.json from embed.FS
        ├─ compute SHA-256 of the manifest
        ├─ extractedPath := $TMPDIR/go-embedded-<name>-<hash[:16]>
        ├─ flock(extractedPath + ".lock")   // serialize peers
        ├─ walk manifest:
        │     • if target exists and Size matches → skip
        │     • else gunzip from embed.FS → write to disk
        │     • replay symlinks through the manifest
        └─ return EmbeddedFiles{extractedPath}
  └─ NewPython(WithPythonHome(extractedPath))
```

The hash suffix in the directory name lets two differently-versioned
binaries on the same machine coexist without stepping on each other.
The flock lock means two copies of the same binary starting at the same
time will not race to extract into the same directory.

### The pieces

`internal/tar.go` is a streaming zstd→tar extractor used at release
time. It handles regular files, directories, and symlinks. Hardlinks
currently error out, which is fine for CPython but worth knowing if
you fork this for a different payload.

`internal/cleanup_python.go` holds the glob-driven keep-list that
trims the stdlib. This is where you tweak things if you want to ship
or drop a particular module.

`embed_util/file_list.go` defines `fileList` and `fileListEntry`.
Each entry records name, mode, size, compression flag, and symlink
target. The full list is what gets serialized into `files.json`.

`embed_util/packer.go` does the release-time packing. It walks the
install tree and compresses each regular file individually using
`compress/gzip` at `BestCompression`. It also writes the
`embed_<os>_<arch>.go` stub.

`embed_util/embedded_files.go` is the runtime extractor. It takes any
`fs.FS`, so the same code extracts both the interpreter and your own
pip-packed packages. Symlinks resolve through the manifest rather than
through the host filesystem, which matters because `embed.FS` cannot
represent symlinks natively.

`python/embedded_python.go` ties `EmbeddedFiles` to a `Python`
interface. The `Python` interface is a thin wrapper around
`exec.Command` that sets `PYTHONHOME` and `PYTHONPATH` for you.
`AddPythonPath(dir)` splices an additional directory in, which is how
you wire pip-packed packages into the interpreter.

`pip/pip_lib.go` and `pip/embed_pip_packages.go` are build-time helpers
that package pip itself (already embedded in `pip/internal/data/`) and
run it against a user's `requirements.txt`. Platform selection uses
pip's `--platform` flag with hardcoded tags that map to the supported
`(goos, goarch)` matrix.

### Per-file gzip vs one big tarball

The obvious alternative to per-file gzip is a single `.tar.zst` blob
that gets extracted on first run. The original authors picked per-file
gzip and I agree with the choice.

First, `embed.FS` lookups are path-based. Per-file entries let the
extractor `stat` each target, compare sizes, and skip writes when the
disk is already in sync with the manifest. The second-run fast path
is a few hundred syscalls and no decompression.

Second, partial extracts degrade gracefully. Kill the process halfway
through first-run extraction and the next run resumes one file at a
time instead of having to redecompress a 100 MB archive.

Third, gzip is in the standard library. zstd would shave 15 to 30
percent off the compressed payload and the packer already pulls in
`klauspost/compress` for the release-time tarball, so switching is
not much work. It is on the [Roadmap](#roadmap); it just hasn't
bubbled to the top.

### python-build-standalone

The actual Python distributions come from
[`astral-sh/python-build-standalone`](https://github.com/astral-sh/python-build-standalone)
(PBS). That project produces fully relocatable, statically-linked,
PGO+LTO CPython builds with pinned OpenSSL, sqlite, libexpat, ncurses,
and so on. Portable CPython is harder than it sounds and they are the
people who have actually solved it.

PBS was originally Gregory Szorc's project under
`indygreg/python-build-standalone`, built to support
[PyOxidizer](https://github.com/indygreg/PyOxidizer). Astral, the team
behind `uv` and `ruff`, took over maintenance in early 2024. It now
underpins `uv python install` and is one of the load-bearing pieces of
the modern Python packaging stack.

`goempy` pulls the `*-pgo+lto-full.tar.zst` build on Unix and the
`*-pgo-full.tar.zst` build on Windows (no LTO on MSVC). For local
development the generator accepts a `--only-platforms=darwin/arm64`
flag so you can iterate without downloading all five builds.

## Supported platforms

| GOOS    | GOARCH | PBS triple                   | LTO | Notes                              |
|---------|--------|------------------------------|-----|------------------------------------|
| linux   | amd64  | `x86_64-unknown-linux-gnu`   | yes | glibc ≥ 2.17 (manylinux_2_17)      |
| linux   | arm64  | `aarch64-unknown-linux-gnu`  | yes | PGO+LTO since PBS 20260414         |
| darwin  | amd64  | `x86_64-apple-darwin`        | yes | macOS 11+                          |
| darwin  | arm64  | `aarch64-apple-darwin`       | yes | macOS 11+ on Apple Silicon         |
| windows | amd64  | `x86_64-pc-windows-msvc`     | no  | non-shared libpython; `.exe` entry |

Not yet wired up: `windows/arm64`, `linux/musl` (amd64 and arm64),
`x86_64_v{2,3,4}` micro-arch Linux builds, Android, Emscripten, and
the free-threaded (PEP 703) variants. PBS has all of those; the
matrix in `release.yml` just does not enumerate them yet.

## Supported Python versions

Each tag is one `(python, pbs)` pair. As of the first `goempy` release
in April 2026, PBS 20260414 ships:

| Python  | Status     | Notes                                                |
|---------|------------|------------------------------------------------------|
| 3.14.4  | primary    | free-threaded builds available but not yet packaged  |
| 3.13.13 | stable     |                                                      |
| 3.12.13 | stable     |                                                      |
| 3.11.15 | stable     |                                                      |
| 3.10.20 | sunsetting | CPython upstream EOL in October 2026                 |

One `release.yml` matrix run produces all of them.

## Release tag scheme

Each release produces one tag per supported Python line. The primary
line (3.14 right now) gets a clean semver tag. The other lines get
valid semver prereleases so that `go get @latest` always lands on
the primary:

| CPython | Tag                  | How to pin                                        |
|---------|----------------------|---------------------------------------------------|
| 3.14.4  | `v0.1.0`             | `go get github.com/tamnd/goempy@latest`           |
| 3.13.13 | `v0.1.0-py3.13.13`   | `go get github.com/tamnd/goempy@v0.1.0-py3.13.13` |
| 3.12.13 | `v0.1.0-py3.12.13`   | `go get github.com/tamnd/goempy@v0.1.0-py3.12.13` |
| 3.11.15 | `v0.1.0-py3.11.15`   | `go get github.com/tamnd/goempy@v0.1.0-py3.11.15` |
| 3.10.20 | `v0.1.0-py3.10.20`   | `go get github.com/tamnd/goempy@v0.1.0-py3.10.20` |

The library version itself lives in the `VERSION` file at the repo
root; the primary Python line lives in `PRIMARY_PYTHON`. Bumping
either bumps every tag in the next release.

## Embedding pip packages

The same packer that produces the interpreter also packs pip-installed
wheels into a per-platform `embed.FS`. In your repo:

```go
// internal/mylib/generate/main.go
package main

import "github.com/tamnd/goempy/pip"

func main() {
	err := pip.CreateEmbeddedPipPackagesForKnownPlatforms(
		"requirements.txt",
		"./data/",
	)
	if err != nil {
		panic(err)
	}
}
```

```go
// internal/mylib/dummy.go
package mylib

//go:generate go run ./generate
```

```
# internal/mylib/requirements.txt
jinja2==3.1.4
```

Then `go generate ./internal/mylib/...`. The generator downloads wheels
for every `(goos, goarch)` in the matrix using
`pip install --platform … --only-binary=:all:` and writes each target
directory out using the same packing logic. At runtime:

```go
libs, _ := embed_util.NewEmbeddedFiles(data.Data, "mylib-pip")
ep.AddPythonPath(libs.GetExtractedPath())

cmd, _ := ep.PythonCmd("-c", "import jinja2; print(jinja2.__version__)")
```

The [`example/`](./example) directory in this repo has a small working
version. [`kluctl/go-jinja2`](https://github.com/kluctl/go-jinja2) has
a much fuller one, and that was where the pattern was first
stress-tested in production.

## Roadmap

Rough order of what I want to tackle next:

1. Fix the manifest integrity check. The runtime "unchanged" path
   compares only `Size()`, which can return a false positive across a
   Python upgrade if a stdlib file happens to keep the same byte
   count. The manifest already carries a content hash; use it.
2. Normalize path separators in the manifest when the packer runs on
   Windows. Currently `filepath.Separator` leaks into `files.json`
   as `\`, which breaks the per-entry lookup at runtime.
3. Switch per-file compression from gzip to zstd. Pure-Go decoder
   via `klauspost/compress/zstd`, already a transitive dependency.
4. Add `windows/arm64` and `linux/musl` (amd64 and arm64) to the
   matrix. PBS ships them.
5. Opt-in free-threaded (PEP 703) builds as a matrix axis.
6. A lazy extract mode: ship a single compressed blob and stream-extract
   on first `PythonCmd` call. Would be an option, not the default; the
   per-file layout is worth keeping for the skip-on-unchanged fast path.
7. Android and iOS triples. PBS 20260414 has Android, and Python 3.14
   promoted it to tier-3.

Contributions welcome.

## Non-goals

In-process Python via CGo is explicitly out of scope. If that is what
you want, go with `go-python/cpy3` or PyOxidizer and make peace with
shipping libpython.

Python-calls-Go or Go-calls-Python bindings are also out of scope.
[`go-python/gopy`](https://github.com/go-python/gopy) is the project
for that, and it composes fine with this one.

A general-purpose `embed.FS` compressor is not something I want
`embed_util` to become, even though it could look like one. The
package is tuned for the particular shape of a CPython install tree:
many small files, some symlinks, no hardlinks, predictable layout.

## Credits

### Upstream: kluctl/go-embed-python

The original work, and the overwhelming majority of the code in this
repository, is
[`kluctl/go-embed-python`](https://github.com/kluctl/go-embed-python).
It was written by Alexander Block
([@codablock](https://github.com/codablock)) and the Kluctl
contributors. The project was extracted from
[`kluctl/kluctl`](https://github.com/kluctl/kluctl), a Kubernetes GitOps
tool where it was originally used to embed Jinja2 templating without a
system Python dependency.

If this library helps you, please go star the upstream repository.
Everything clever here is theirs.

This fork adds, relative to the last upstream release
(`v0.0.0-3.13.1-20241219-1` in the old scheme):

- Python 3.14.4 and python-build-standalone 20260414.
- Fix for the Windows PBS triple rename from
  `pc-windows-msvc-shared-pgo-full` to `pc-windows-msvc-pgo-full`.
  Without it, the 3.14 download silently 404'd on Windows.
- Upgrade of `linux/arm64` to PGO+LTO. PBS used to ship only
  `lto-full` for aarch64 Linux and now ships `pgo+lto-full`.
- Go toolchain 1.19 to 1.24. `sirupsen/logrus` replaced by the
  standard library's `log/slog`.
- pip 24.3.1 to 25.2, with `setuptools>=75` and `wheel>=0.45` pinned
  explicitly so that `get-pip.py` cannot drift to older versions.
- A `--only-platforms` flag on `python/generate` so you can build
  one platform locally instead of all five.
- Docs rewrite.

Individual file headers keep their original attribution. The project
stays Apache-2.0. Nothing is relicensed.

### python-build-standalone

The CPython builds we redistribute come from
[`astral-sh/python-build-standalone`](https://github.com/astral-sh/python-build-standalone).
Before Astral took over, the project was
[`indygreg/python-build-standalone`](https://github.com/indygreg/python-build-standalone)
and was maintained by Gregory Szorc. Portable CPython is a hard
problem and this project is one of the quiet load-bearing pieces of
the Python packaging ecosystem.

## Related projects

Things you might want instead, or alongside:

- [`kluctl/go-embed-python`](https://github.com/kluctl/go-embed-python).
  Upstream. If you are on Python 3.13 or earlier and the last upstream
  release meets your needs, use this.
- [`kluctl/go-jinja2`](https://github.com/kluctl/go-jinja2). Jinja2
  templating for Go, implemented by driving an embedded Python
  subprocess. The best worked example of how to wire a pip-packed
  `embed.FS` into an `EmbeddedPython`.
- [`kluctl/kluctl`](https://github.com/kluctl/kluctl). Kubernetes
  GitOps tool. Where this code originally came from.
- [`astral-sh/python-build-standalone`](https://github.com/astral-sh/python-build-standalone).
  The portable CPython builds we redistribute. Read their release
  notes before upgrading.
- [`astral-sh/uv`](https://github.com/astral-sh/uv). Rust-based Python
  package and project manager. Also consumes PBS under the hood. If
  you are reaching for Python from Rust, `uv` does roughly what
  `goempy` does from Go, minus the subprocess model.
- [`indygreg/PyOxidizer`](https://github.com/indygreg/PyOxidizer) and
  its [`pyembed`](https://github.com/indygreg/PyOxidizer/tree/main/pyembed)
  crate. The other approach: in-process CPython linked into a single
  binary. Mostly unmaintained now.
- [`go-python/gopy`](https://github.com/go-python/gopy). Generator for
  Go bindings to Python packages. Different problem; complementary.
- [`go-python/cpy3`](https://github.com/go-python/cpy3). Direct CGo
  bindings to libpython. What you use if you truly want in-process
  Python and can live with the deployment pain.
- [`pypa/cibuildwheel`](https://github.com/pypa/cibuildwheel). Useful
  reading for the cross-platform CI matrix patterns we imitate.

## License

Apache-2.0, same as upstream. See [`LICENSE`](./LICENSE).

Original copyright: Kluctl contributors. Fork maintenance: Duc-Tam
Nguyen &lt;tamnd@liteio.dev&gt;.
