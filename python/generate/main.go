package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gobwas/glob"
	"github.com/klauspost/compress/zstd"
	"github.com/tamnd/goempy/embed_util"
	"github.com/tamnd/goempy/internal"
)

var (
	pythonStandaloneVersion = flag.String("python-standalone-version", "", "specify the python-standalone version. Check https://github.com/astral-sh/python-build-standalone/releases/ for available options.")
	pythonVersion           = flag.String("python-version", "", "specify the python version.")
	preparePath             = flag.String("prepare-path", filepath.Join(os.TempDir(), "python-download"), "specify the path where the python executables are downloaded and prepared. automatically creates a temporary directory if unset")
	runPrepare              = flag.Bool("prepare", true, "if set, python executables will be downloaded and prepared for packing at the configured path")
	runPack                 = flag.Bool("pack", true, "if set, previously prepared python executables will be packed into their redistributable form")
	onlyPlatforms           = flag.String("only-platforms", "", "comma-separated list of os/arch pairs to generate (e.g. 'linux/amd64,darwin/arm64'); default is all supported")
	pythonVersionBase       string
)

var archMapping = map[string]string{
	"amd64": "x86_64",
	"386":   "i686",
	"arm64": "aarch64",
}

var removeLibs = []string{
	"ensurepip",
	"idlelib",
	"lib2to3",
	"pydoc_data",
	"site-packages",
	"test",
	"turtledemo",
	"bin", // not really a library, but erroneously installed by jsonpath_ng
}

var keepNixPatterns = []glob.Glob{
	glob.MustCompile("bin/**"),
	glob.MustCompile("lib/*.so*"),
	glob.MustCompile("lib/*.dylib"),
	glob.MustCompile("lib/python3.*/**"),
}
var keepWinPatterns = []glob.Glob{
	glob.MustCompile("Lib/**"),
	glob.MustCompile("DLLs/**"),
	glob.MustCompile("*.dll"),
	glob.MustCompile("*.exe"),
}

var downloadLock sync.Mutex

func main() {
	flag.Parse()

	if *pythonVersion == "" || *pythonStandaloneVersion == "" {
		slog.Error("missing flags")
		os.Exit(1)
	}

	slog.Info("generating",
		"python-standalone-version", *pythonStandaloneVersion,
		"python-version", *pythonVersion)

	pythonVersionBase = strings.Join(strings.Split(*pythonVersion, ".")[0:2], ".")

	targetPath := "./python/internal/data"

	var wg sync.WaitGroup

	type job struct {
		os           string
		arch         string
		dist         string
		keepPatterns []glob.Glob
	}

	jobs := []job{
		{"linux", "amd64", "unknown-linux-gnu-pgo+lto-full", keepNixPatterns},
		{"linux", "arm64", "unknown-linux-gnu-pgo+lto-full", keepNixPatterns},
		{"darwin", "amd64", "apple-darwin-pgo+lto-full", keepNixPatterns},
		{"darwin", "arm64", "apple-darwin-pgo+lto-full", keepNixPatterns},
		{"windows", "amd64", "pc-windows-msvc-pgo-full", keepWinPatterns},
	}

	if *onlyPlatforms != "" {
		want := map[string]bool{}
		for _, p := range strings.Split(*onlyPlatforms, ",") {
			want[strings.TrimSpace(p)] = true
		}
		filtered := jobs[:0]
		for _, j := range jobs {
			if want[j.os+"/"+j.arch] {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
		if len(jobs) == 0 {
			slog.Error("no platforms matched filter", "filter", *onlyPlatforms)
			os.Exit(1)
		}
	}

	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			if *runPrepare {
				downloadAndPrepare(j.os, j.arch, j.dist, j.keepPatterns)
			}
			if *runPack {
				packPrepared(j.os, j.arch, j.dist, targetPath)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func downloadAndPrepare(osName string, arch string, dist string, keepPatterns []glob.Glob) {
	downloadPath := download(osName, arch, dist)

	extractPath := downloadPath + ".extracted"
	err := os.RemoveAll(extractPath)
	if err != nil {
		panic(err)
	}

	extract(downloadPath, extractPath)

	installPath := filepath.Join(extractPath, "python", "install")

	var libPath string
	if osName == "windows" {
		libPath = filepath.Join(installPath, "Lib")
	} else {
		libPath = filepath.Join(installPath, "lib", fmt.Sprintf("python%s", pythonVersionBase))
	}

	for _, lib := range removeLibs {
		_ = os.RemoveAll(filepath.Join(libPath, lib))
	}

	err = internal.CleanupPythonDir(installPath, keepPatterns)
	if err != nil {
		panic(err)
	}
}

func packPrepared(osName string, arch string, dist string, targetPath string) {
	extractPath := generateDownloadPath(arch, dist) + ".extracted"
	installPath := filepath.Join(extractPath, "python", "install")
	err := embed_util.CopyForEmbed(filepath.Join(targetPath, fmt.Sprintf("%s-%s", osName, arch)), installPath)
	if err != nil {
		panic(err)
	}

	err = embed_util.WriteEmbedGoFile(targetPath, osName, arch)
	if err != nil {
		panic(err)
	}

	f, err := os.Create(filepath.Join(targetPath, "PYTHON_VERSION"))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "PYTHON_VERSION=%q\nPYTHON_STANDALONE_VERSION=%q\n", *pythonVersion, *pythonStandaloneVersion)
	if err != nil {
		panic(err)
	}
}

func generateDownloadPath(arch string, dist string) string {
	pythonArch, ok := archMapping[arch]
	if !ok {
		slog.Error("arch not supported", "arch", arch)
		os.Exit(1)
	}
	fname := fmt.Sprintf("cpython-%s+%s-%s-%s.tar.zst", *pythonVersion, *pythonStandaloneVersion, pythonArch, dist)
	return filepath.Join(*preparePath, fname)
}

func download(osName string, arch string, dist string) string {
	downloadLock.Lock()
	defer downloadLock.Unlock()

	downloadPath := generateDownloadPath(arch, dist)
	fname := filepath.Base(downloadPath)
	downloadUrl := fmt.Sprintf("https://github.com/astral-sh/python-build-standalone/releases/download/%s/%s", *pythonStandaloneVersion, fname)

	if _, err := os.Stat(downloadPath); err == nil {
		slog.Info("skipping download", "url", downloadUrl)
		return downloadPath
	}

	err := os.MkdirAll(filepath.Dir(downloadPath), 0o755)
	if err != nil {
		slog.Error("mkdirs failed", "err", err)
		os.Exit(1)
	}
	slog.Info("downloading", "url", downloadUrl)

	r, err := http.Get(downloadUrl)
	if err != nil {
		slog.Error("download failed", "err", err)
		os.Exit(1)
	}
	if r.StatusCode == http.StatusNotFound {
		slog.Error("404 not found", "url", downloadUrl)
		os.Exit(1)
	}
	defer r.Body.Close()

	fileData, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("reading response failed", "err", err)
		os.Exit(1)
	}

	err = os.WriteFile(downloadPath, fileData, 0o640)
	if err != nil {
		slog.Error("writing file failed", "err", err)
		os.Remove(downloadPath)
		os.Exit(1)
	}

	return downloadPath
}

func extract(archivePath string, targetPath string) string {
	f, err := os.Open(archivePath)
	if err != nil {
		slog.Error("opening file failed", "err", err)
		os.Exit(1)
	}
	defer f.Close()

	z, err := zstd.NewReader(f)
	if err != nil {
		slog.Error("decompression failed", "err", err)
		os.Exit(1)
	}
	defer z.Close()

	slog.Info("decompressing", "path", archivePath)
	err = internal.ExtractTarStream(z, targetPath)
	if err != nil {
		slog.Error("decompression failed", "err", err)
		os.Exit(1)
	}

	return targetPath
}
