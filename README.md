# goempy — Embedded Python 3.14 for Go

`goempy` ships a ready-to-run CPython interpreter inside your Go binary. No
CGo, no system Python, no external runtime — just `go get`, embed, and exec.

It is a modernized fork of [`kluctl/go-embed-python`](https://github.com/kluctl/go-embed-python)
tracking Python 3.14 and the Astral [`python-build-standalone`](https://github.com/astral-sh/python-build-standalone)
releases.

## Quick start

```go
import (
	"os"
	"github.com/tamnd/goempy/python"
)

func main() {
	ep, err := python.NewEmbeddedPython("example")
	if err != nil {
		panic(err)
	}
	defer ep.Cleanup()

	cmd, err := ep.PythonCmd("-c", "print('hello from embedded python')")
	if err != nil {
		panic(err)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
```

## Supported platforms

| OS      | Arch  | PBS triple                         |
|---------|-------|-------------------------------------|
| linux   | amd64 | `x86_64-unknown-linux-gnu`          |
| linux   | arm64 | `aarch64-unknown-linux-gnu`         |
| darwin  | amd64 | `x86_64-apple-darwin`               |
| darwin  | arm64 | `aarch64-apple-darwin`              |
| windows | amd64 | `x86_64-pc-windows-msvc` (non-shared) |

Planned: `windows/arm64`, `linux/musl`, free-threaded (PEP 703) builds.

## Supported Python versions

Primary: **3.14.4**. Also shipped: 3.13.13, 3.12.13, 3.11.15, 3.10.20.

## Releases

Tag format: `v0.0.0-<python>-<pbs>-<build>`, e.g. `v0.0.0-3.14.4-20260414-1`.
The `v0.0.0` prefix is intentional — this library does not follow semver.
The meaningful identifier is the `<python>-<pbs>` pair. Pin exactly.

> Dependabot and similar tools may mis-resolve upgrades against this scheme.
> Review Python version bumps manually.

## How it works

1. At release time, `python/generate` downloads each `(python, pbs, platform)`
   triple from python-build-standalone, strips unused stdlib, and writes a
   per-platform directory to `python/internal/data/<os>-<arch>/`.
2. `embed_util.CopyForEmbed` compresses each file (gzip) and emits
   `//go:embed` targets with build constraints so only the host's bytes are
   linked into the final binary.
3. At runtime, `python.NewEmbeddedPython` extracts its `embed.FS` into
   `$TMPDIR/go-embedded-<name>-<hash>`, guarded by `flock`, and returns an
   `exec.Cmd` factory.

## Embedding Python libraries

Create `internal/mylib/generate/main.go`:

```go
package main

import "github.com/tamnd/goempy/pip"

func main() {
	err := pip.CreateEmbeddedPipPackagesForKnownPlatforms("requirements.txt", "./data/")
	if err != nil {
		panic(err)
	}
}
```

Add `//go:generate go run ./generate` and a `requirements.txt` next to it, then
`go generate ./...`. The generated `data.Data` `embed.FS` is passed to
`embed_util.NewEmbeddedFiles()` and wired into the interpreter via
`AddPythonPath`.

A working example lives in [`example/`](./example).

## Upgrading Python

Edit `.github/workflows/release.yml`:

```yaml
PYTHON_STANDALONE_VERSIONS: ["20260414"]
PYTHON_VERSIONS: ["3.10.20", "3.11.15", "3.12.13", "3.13.13", "3.14.4"]
```

Open a PR — CI will build the full matrix and tag on merge.

## Why fork?

Upstream `kluctl/go-embed-python` has been largely dormant since early 2025.
The Python 3.14 upgrade PR has sat open since February. `goempy` picks up:

- Python 3.14.4 + python-build-standalone `20260414`
- Windows dist name fix (PBS dropped the `shared-` infix)
- Go 1.24 toolchain, `log/slog` (drop `logrus`)
- pip 25.2, pinned `get-pip.py`

See [spec 0967](../../notes/Spec/0900/0967_go_embed_python.md) for the upgrade
rationale and roadmap.

## License

Apache-2.0 — same as upstream. See [`LICENSE`](./LICENSE).

Original authorship: kluctl contributors. Modernization fork: Duc-Tam Nguyen
&lt;tamnd@liteio.dev&gt;.
