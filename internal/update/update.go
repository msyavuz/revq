// Package update finds newer revq releases on GitHub and installs them over
// the running binary.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const repo = "msyavuz/revq"

var client = &http.Client{Timeout: 2 * time.Minute}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "revq-update")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// Latest returns the tag of the newest full release, such as "v0.2.0".
func Latest(ctx context.Context) (string, error) {
	body, err := get(ctx, "https://api.github.com/repos/"+repo+"/releases/latest")
	if err != nil {
		return "", err
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	if r.TagName == "" {
		return "", errors.New("no release found")
	}
	return r.TagName, nil
}

// parse turns "v1.2.3" into its three numbers. Pre-release and unversioned
// builds ("v1.2.3-rc.1", "dev") don't parse.
func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether latest is a later version than current. An
// unversioned build never counts as out of date.
func Newer(latest, current string) bool {
	l, ok1 := parse(latest)
	c, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// Apply downloads the given release for this machine, checks it against the
// release's checksum file, and replaces the running executable. The caller
// restarts the service afterwards.
func Apply(ctx context.Context, tag string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	name := fmt.Sprintf("revq_%s_%s_%s", tag, runtime.GOOS, runtime.GOARCH)
	base := "https://github.com/" + repo + "/releases/download/" + tag + "/"

	sums, err := get(ctx, base+"checksums.txt")
	if err != nil {
		return "", err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name+".tar.gz" {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("release %s has no build for %s/%s", tag, runtime.GOOS, runtime.GOARCH)
	}
	archive, err := get(ctx, base+name+".tar.gz")
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return "", errors.New("download does not match the release checksum; nothing was changed")
	}

	bin, err := extract(archive, name+"/revq")
	if err != nil {
		return "", err
	}
	// Write next to the target and rename, so a failed update never leaves a
	// half-written binary in place.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".revq-update-*")
	if err != nil {
		return "", fmt.Errorf("can't write to %s (run as a user who can, usually root): %w", filepath.Dir(exe), err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", err
	}
	return exe, nil
}

func extract(archive []byte, path string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in the release archive", path)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == path && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}
