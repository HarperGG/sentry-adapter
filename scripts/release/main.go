// Command release builds Linux binaries and writes their SHA-256 checksums.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var safeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func main() {
	goBinary := flag.String("go", "go", "Go executable")
	version := flag.String("version", "dev", "release version")
	flag.Parse()
	if !safeVersion.MatchString(*version) {
		fail("version must contain only letters, digits, dots, underscores, and hyphens")
	}
	distDir := filepath.Join("dist", *version)
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		fail("create %s: %v", distDir, err)
	}

	var checksums strings.Builder
	for _, arch := range []string{"amd64", "arm64"} {
		for _, product := range []struct {
			name, packagePath string
		}{
			{"sentry-adapter", "./cmd/adapter"},
			{"teambition-probe", "./cmd/teambition-probe"},
		} {
			name := fmt.Sprintf("%s_%s_linux_%s", product.name, *version, arch)
			path := filepath.Join(distDir, name)
			cmd := exec.Command(*goBinary, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", path, product.packagePath)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			fmt.Printf("building %s\n", name)
			if err := cmd.Run(); err != nil {
				fail("build %s: %v", name, err)
			}
			f, err := os.Open(path)
			if err != nil {
				fail("open %s: %v", name, err)
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			closeErr := f.Close()
			if copyErr != nil {
				fail("hash %s: %v", name, copyErr)
			}
			if closeErr != nil {
				fail("close %s: %v", name, closeErr)
			}
			fmt.Fprintf(&checksums, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), name)
		}
	}
	if err := os.WriteFile(filepath.Join(distDir, "SHA256SUMS"), []byte(checksums.String()), 0o644); err != nil {
		fail("write checksums: %v", err)
	}
	fmt.Printf("wrote %s/SHA256SUMS\n", filepath.ToSlash(distDir))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
