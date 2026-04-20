# goempy

`goempy` bakes a working CPython 3.14 interpreter into your Go binary. Call
`python.NewEmbeddedPython("myapp")`, get back an `*exec.Cmd` factory, and run
Python code without any Python on the host — no `apt install python3`, no
PyInstaller tricks, no CGo.

It is a fork of [`kluctl/go-embed-python`](https://github.com/kluctl/go-embed-python)
that I started in April 2026 because upstream had gone quiet and Python 3.14
had just shipped. All of the hard design work — the per-file gzip layout, the
flock-guarded extraction, the pip integration — is the original authors'.
Everything in this tree is under Apache-2.0 and attributed accordingly (see
[Credits](#credits)).

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

Build that program with `go get github.com/tamnd/goempy@v0.0.0-3.14.4-20260414-1`
and run it. The first invocation extracts about 22 MB of Python into
`$TMPDIR/go-embedded-python-hello-<hash>/` and then executes
`bin/python3 -c …`. Subsequent invocations reuse the same directory — the
integrity check looks at file size and skips the copy if nothing has changed.

## Why this exists

If you want to run Python from Go you have a few options, and most of them
hurt:

- **CGo + libpython** via
  [`go-python/cpy3`](https://github.com/go-python/cpy3) or similar.
  Requires the right libpython on the host at runtime. Cross-compiling is a
  nightmare. Stability is fragile under load.
- **A sidecar process** you ship alongside your binary. You own the
  installation story for every platform.
- **[PyOxidizer](https://github.com/indygreg/PyOxidizer) /
  [pyembed](https://github.com/indygreg/PyOxidizer/tree/main/pyembed)**.
  Rust-centric, in-process, heavyweight, and the project is effectively
  abandoned.

`kluctl/go-embed-python` took a different route, which this fork inherits:
**embed the entire stdlib + interpreter into the Go binary**, extract on
first run, and call it as a subprocess. No CGo. No host dependencies. Cross
compilation is just `GOOS=linux GOARCH=arm64 go build`.

The size cost is real — a single-platform binary gains roughly 25–30 MB of
compressed Python — but for CLI tools, operators, and GitOps controllers
that want to embed templating engines or pure-Python libraries, it is the
cleanest option I have found.

## Architecture

```
┌───────────────────────── release-time (CI) ──────────────────────────┐
│                                                                      │
│   python/generate  ─┬─►  download PBS tarball   (─> tar.zst)          │
│   (one per platform)│                                                 │
│                     ├─►  zstd → tar → install/ tree                   │
│                     │                                                 │
│                     ├─►  strip stdlib: test, idlelib, lib2to3, ...    │
│                     │                                                 │
│                     └─►  embed_util.CopyForEmbed                      │
│                              │                                        │
│                              ▼                                        │
│                 python/internal/data/<goos>-<goarch>/                 │
│                    ├── bin/python3.gz   (per-file gzip -9)            │
│                    ├── lib/python3.14/**/*.gz                         │
│                    ├── files.json        (manifest + content hash)    │
│                    └── symlinks preserved via manifest                │
│                                                                      │
│   pip/generate   ──► pip install -r requirements.txt --platform …     │
│                      into python/internal/data/pip/  (same layout)    │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
               │                                       │
               └──────────── git tag ──────────────────┘
               v0.0.0-<python>-<pbs>-<build>

┌───────────────────────── build-time (user's app) ───────────────────┐
│                                                                     │
│   //go:embed all:linux-amd64        (build constraint per file)     │
│   var _data embed.FS                                                │
│   var Data, _ = fs.Sub(_data, "linux-amd64")                        │
│                                                                     │
│   ► go link only embeds the bytes for GOOS/GOARCH of the build.     │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘

┌───────────────────────── runtime (user's app) ──────────────────────┐
│                                                                     │
│   python.NewEmbeddedPython(name)                                    │
│       └─► embed_util.NewEmbeddedFiles(data.Data, "python-"+name)    │
│              ├── read files.json from embed.FS                      │
│              ├── compute SHA-256 hash of manifest                   │
│              ├── extractedPath := $TMPDIR/go-embedded-<name>-<hash> │
│              ├── flock(extractedPath + ".lock")   -- crash-safe     │
│              ├── for each entry in manifest:                        │
│              │     • if file exists and Size matches → skip         │
│              │     • else gunzip from embed.FS → write to disk      │
│              │     • replay symlinks from manifest                  │
│              └── return EmbeddedFiles{extractedPath}                │
│                                                                     │
│       └─► NewPython(WithPythonHome(extractedPath))                  │
│              returns an *exec.Cmd factory that sets PYTHONHOME and  │
│              PYTHONPATH for you before exec.                        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### The pieces

`internal/tar.go`
 : Streaming zstd → tar extractor used at release time. Handles regular
   files, directories, symlinks. Hardlinks currently raise an error (see
   [#7](#roadmap)).

`internal/cleanup_python.go`
 : Applies a glob-based keep-list to the extracted PBS install tree,
   removing test suites, documentation, IDLE, tkinter demos, and other
   stdlib weight we do not want to ship.

`embed_util/file_list.go`
 : Defines `fileList` / `fileListEntry`. Each entry records mode, size,
   compression flag, and symlink target. The full list is serialized to
   `files.json` and hashed to form the extraction directory suffix.

`embed_util/packer.go`
 : Walks the cleaned install tree, gzip-compresses each regular file at
   `BestCompression`, and writes `*.gz` alongside a manifest. Also
   generates the per-platform `embed_<os>_<arch>.go` stub that the Go
   compiler consumes via `//go:embed all:<os>-<arch>`.

`embed_util/embedded_files.go`
 : The runtime extractor. Takes any `fs.FS` (so you can also embed your
   own pip packages), resolves symlinks through the manifest, and writes
   to a per-hash directory under `$TMPDIR`. A `gofrs/flock` lock serializes
   concurrent extractions so that multiple processes in the same host
   cannot race.

`python/embedded_python.go`
 : The user-facing `EmbeddedPython` type. Couples an `EmbeddedFiles`
   (extraction) with a `Python` (exec.Cmd factory). `PYTHONHOME` is wired
   up automatically; call `AddPythonPath(dir)` to splice additional
   directories — normally the extracted path of a pip-packed `embed.FS`.

`pip/*`
 : Build-time helpers that shell out to the embedded pip (25.2 at the
   time of writing) with `--platform` filters to fetch cross-platform
   wheels into `./data/<goos>-<goarch>/`. You wire this into your own
   project with a `//go:generate go run ./generate` stub.

### Why per-file gzip instead of a single tarball

The obvious alternative is a single `.tar.zst` blob extracted on first
run. The original authors chose per-file gzip and that choice still pays
off:

1. `embed.FS` lookups are path-based. Per-file entries let runtime check
   whether a file already exists on disk at the right size and skip it
   — the "unchanged" fast path turns second-run extraction into a few
   hundred `stat` calls.
2. Partial extracts fail gracefully. If the process is killed in the
   middle of extraction, the next run resumes per-file without having
   to redecompress a 100 MB archive.
3. gzip is in the Go standard library. zstd would shave 15–30 % off the
   compressed size — there is an open thought in [`#roadmap`](#roadmap)
   to swap it — but it is not free; the packer already depends on
   `klauspost/compress` for the release-time tarball, so the trade-off
   is mostly about runtime decompressor choice.

### python-build-standalone

The Python distributions themselves come from
[`astral-sh/python-build-standalone`](https://github.com/astral-sh/python-build-standalone)
(PBS), which ships fully relocatable, statically-linked, PGO+LTO CPython
builds with pinned OpenSSL, sqlite, libexpat, etc. That project does
almost all of the interesting work — CPython builds that are actually
portable across glibc versions are not easy.

PBS was originally maintained by Gregory Szorc as part of the
[PyOxidizer](https://github.com/indygreg/PyOxidizer) effort under
`indygreg/python-build-standalone`. Astral (the `uv` / `ruff` folks)
took over the project in early 2024 and it is now the foundation for
`uv python install`, Astral's own Python installer. `goempy` rides on
top of the same releases.

`python/generate/main.go` downloads the `*-pgo+lto-full.tar.zst` (or
`*-pgo-full.tar.zst` on Windows — PBS does not do LTO on MSVC builds)
for each (`goos`, `goarch`) in the matrix. A `--only-platforms` flag is
available for local development.

## Supported platforms

| GOOS    | GOARCH | PBS triple                         | LTO | Notes                                    |
|---------|--------|-------------------------------------|-----|------------------------------------------|
| linux   | amd64  | `x86_64-unknown-linux-gnu`          | yes | glibc ≥ 2.17 (manylinux_2_17)            |
| linux   | arm64  | `aarch64-unknown-linux-gnu`         | yes | upgraded to pgo+lto in PBS 20260414      |
| darwin  | amd64  | `x86_64-apple-darwin`               | yes | macOS 11+ (x86_64)                       |
| darwin  | arm64  | `aarch64-apple-darwin`              | yes | macOS 11+ (Apple Silicon)                |
| windows | amd64  | `x86_64-pc-windows-msvc`            | no  | non-shared libpython; `.exe` entry point |

Not yet wired up (PBS has the artifacts, just not in this fork's matrix):
`windows/arm64`, `linux/musl-{amd64,arm64}`, Linux micro-arch variants
(`x86_64_v{2,3,4}`), Android, Emscripten, and the free-threaded (PEP 703)
builds. See [Roadmap](#roadmap).

## Supported Python versions

Each release tag is a `(python, pbs)` pair. As of the first `goempy`
release (April 2026), PBS 20260414 ships:

| Python  | Status   | Notes                                                  |
|---------|----------|---------------------------------------------------------|
| 3.14.4  | primary  | PEP 703 GIL-disable available via free-threaded builds |
| 3.13.13 | stable   |                                                         |
| 3.12.13 | stable   |                                                         |
| 3.11.15 | stable   |                                                         |
| 3.10.20 | sunsetting | CPython upstream EOL October 2026                    |

All of them build from a single `release.yml` matrix.

## Release tags

Tag format: `v0.0.0-<python>-<pbs>-<build>`. Example:

```
v0.0.0-3.14.4-20260414-1
        │      │       └─ build number (for re-releases of the same pair)
        │      └────────── python-build-standalone release date tag
        └───────────────── CPython version
```

The leading `v0.0.0` is intentional — this library does not follow semver
and probably never will. The meaningful identifier is the
`<python>-<pbs>` suffix. `go get` against a specific tag to pin. Dependabot
and similar tools may mis-resolve upgrades against this scheme; review
Python version bumps manually.

## Embedding pip packages

The same machinery that packs CPython also packs pip-installed
dependencies into a per-platform `embed.FS`. Inside your repo:

```go
// internal/mylib/generate/main.go
package main

import "github.com/tamnd/goempy/pip"

func main() {
	if err := pip.CreateEmbeddedPipPackagesForKnownPlatforms(
		"requirements.txt",
		"./data/",
	); err != nil {
		panic(err)
	}
}
```

```
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
`pip install --platform … --only-binary=:all:`, packs each target directory
the same way the interpreter is packed, and emits a `data.Data` `embed.FS`
per platform. At runtime:

```go
libs, _ := embed_util.NewEmbeddedFiles(data.Data, "mylib-pip")
ep.AddPythonPath(libs.GetExtractedPath())
cmd, _ := ep.PythonCmd("-c", "import jinja2; print(jinja2.__version__)")
```

A working example is the [`example/`](./example) directory in this repo,
and a much more complete one lives in
[`kluctl/go-jinja2`](https://github.com/kluctl/go-jinja2) (which is where
this pattern was originally battle-tested).

## Roadmap

Things I want to do, in roughly decreasing priority:

1. **Fix the manifest integrity check.** Current "unchanged" fast path
   compares only `Size()`. Across a 3.13 → 3.14 upgrade, same-named
   stdlib files can have identical sizes and stale bytes on disk survive
   the check. Use the per-entry content hash that already exists in
   `files.json`.
2. **Windows path normalization.** When the packer runs on Windows,
   `filepath.Separator` leaks into `files.json` entries as `\`. Force
   `/` everywhere in the manifest. (Carries intent of upstream PR #50.)
3. **Per-file zstd** instead of gzip. Pure-Go decoder via
   `klauspost/compress/zstd`; saves 15–30 % of embedded bytes.
4. **`windows/arm64`, `linux/musl-{amd64,arm64}`** in the matrix.
5. **Free-threaded (PEP 703) variants** as an opt-in matrix axis. PBS
   already ships `*-freethreaded+pgo-full.tar.zst` everywhere that
   matters.
6. **Lazy extract**: a single compressed blob + streaming extract on
   first `PythonCmd` call. Optional mode — the per-file layout stays
   default for the fast-skip path.
7. **Android / iOS triples**. PBS 20260414 has Android, and 3.14 makes
   it tier-3.

Contributions welcome.

## Non-goals

- **In-process Python via CGo.** Explicitly out of scope. If you want
  that, use `go-python/cpy3` or PyOxidizer and make peace with libpython.
- **Python → Go bindings.** Use
  [`go-python/gopy`](https://github.com/go-python/gopy) for that.
- **A general-purpose `embed.FS` compressor.** The `embed_util` package
  is not trying to be one, even though it could look like one. It is
  tuned for (large, many-file, partially-symlinked Python trees).

## Credits

### Upstream authors

All of the design and the vast majority of the code in this repository
come from the original
[`kluctl/go-embed-python`](https://github.com/kluctl/go-embed-python),
authored by Alexander Block ([@codablock](https://github.com/codablock))
and contributors in the Kluctl organization. The project was extracted
from [`kluctl/kluctl`](https://github.com/kluctl/kluctl), a Kubernetes
GitOps tool, where it was used to ship Jinja2 templating without
depending on a system Python. If this library helps you, star the
[upstream repo](https://github.com/kluctl/go-embed-python) first.

This fork adds:

- Python 3.14.4 + python-build-standalone `20260414`
- Fix for the Windows PBS triple rename (`pc-windows-msvc-shared-pgo-full`
  → `pc-windows-msvc-pgo-full`) that silently broke the 3.14 download path
- Upgrade of `linux/arm64` to PGO+LTO (PBS now ships it)
- Go toolchain 1.19 → 1.24, `log/slog` in place of `sirupsen/logrus`
- pip 24.3.1 → 25.2, with explicit `setuptools>=75` / `wheel>=0.45` pins
- `--only-platforms` on the generator for scoped local builds
- Docs rewritten

Individual file headers retain their original attribution and Apache-2.0
licensing. Nothing in this fork is relicensed.

### python-build-standalone

The Python distributions themselves are
[`astral-sh/python-build-standalone`](https://github.com/astral-sh/python-build-standalone),
originally
[`indygreg/python-build-standalone`](https://github.com/indygreg/python-build-standalone)
by Gregory Szorc. Astral took maintenance over in 2024 and it is now one
of the two or three most important projects in the Python packaging
ecosystem. Everything in this fork rests on their work.

## Related projects

Things you might want instead, or alongside:

- **[kluctl/go-embed-python](https://github.com/kluctl/go-embed-python)**
  — upstream. Use this if you are on Python ≤ 3.13 and the last upstream
  release meets your needs.
- **[kluctl/go-jinja2](https://github.com/kluctl/go-jinja2)** — Jinja2
  templating for Go, implemented by driving an embedded Python
  subprocess. Best real-world consumer of `go-embed-python` and a useful
  reference for how to wire pip-embedded libraries into it.
- **[kluctl/kluctl](https://github.com/kluctl/kluctl)** — the GitOps
  tool where this code originated.
- **[astral-sh/python-build-standalone](https://github.com/astral-sh/python-build-standalone)**
  — the portable CPython builds we redistribute. Read their release
  notes before upgrading.
- **[astral-sh/uv](https://github.com/astral-sh/uv)** — a Rust-based
  Python package / project manager that also consumes PBS. Conceptual
  sibling: if you are a Rust shop, `uv` does for Rust binaries what
  `goempy` does for Go binaries (minus the CGo-free subprocess model).
- **[indygreg/PyOxidizer](https://github.com/indygreg/PyOxidizer)** and
  **[pyembed](https://github.com/indygreg/PyOxidizer/tree/main/pyembed)**
  — the other approach: in-process CPython linked into a single binary.
  Largely unmaintained at this point.
- **[go-python/gopy](https://github.com/go-python/gopy)** — generate Go
  bindings for Python packages. Different problem; they complement each
  other.
- **[go-python/cpy3](https://github.com/go-python/cpy3)** — direct CGo
  bindings to libpython. What you use if you really want to call Python
  in-process and do not care about deployment pain.
- **[cibuildwheel](https://github.com/pypa/cibuildwheel)** — worth a
  read for the cross-platform CI matrix patterns we imitate.

## License

Apache-2.0, the same as upstream. See [`LICENSE`](./LICENSE).

Original copyright: Kluctl contributors. Fork maintenance: Duc-Tam
Nguyen &lt;tamnd@liteio.dev&gt;.
