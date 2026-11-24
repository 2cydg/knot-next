package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	releaseAPI       = "https://api.github.com/repos/2cydg/knot-next/releases/latest"
	releaseDownloads = "https://github.com/2cydg/knot-next/releases"
	metadataLimit    = 1 << 20
	binaryLimit      = 128 << 20
)

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*)?(\+[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*)?$`)

// compareStable compares a local version with a validated stable release tag.
func compareStable(version, tag string) (int, error) {
	local, remote := versionPattern.FindStringSubmatch(version), versionPattern.FindStringSubmatch(tag)
	if local == nil || remote == nil || remote[4] != "" {
		return 0, errors.New("invalid version")
	}
	for i := 1; i <= 3; i++ {
		if len(local[i]) != len(remote[i]) {
			return len(local[i]) - len(remote[i]), nil
		}
		if cmp := strings.Compare(local[i], remote[i]); cmp != 0 {
			return cmp, nil
		}
	}
	if local[4] != "" {
		return -1, nil
	}
	return 0, nil
}

func validRelease(tag string) bool {
	parts := versionPattern.FindStringSubmatch(tag)
	return parts != nil && strings.HasPrefix(tag, "v") && parts[4] == "" && parts[5] == ""
}

func developmentBuild(version string) bool {
	parts := versionPattern.FindStringSubmatch(version)
	return parts == nil || parts[4] == "-dev" || strings.HasPrefix(parts[4], "-dev.")
}

type source struct {
	api       string
	downloads string
	client    *http.Client
}

func officialSource() source {
	base := strings.TrimRight(os.Getenv("KNOT_BASE_URL"), "/")
	if base == "" {
		base = releaseDownloads
	}
	return source{api: releaseAPI, downloads: base, client: &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("invalid release redirect")
			}
			return nil
		},
	}}
}

func assetName(component string) string {
	return "knot-" + component + "-" + runtime.GOOS + "-" + runtime.GOARCH + executableSuffix
}

func (s source) latest(ctx context.Context) (string, error) {
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows") ||
		(runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", errors.New("no release assets for this platform")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	body, err := s.get(ctx, s.api, metadataLimit)
	if err != nil {
		return "", err
	}
	defer body.Close()
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(body, metadataLimit)).Decode(&release); err != nil {
		return "", fmt.Errorf("read release metadata: %w", err)
	}
	if !validRelease(release.Tag) || release.Draft || release.Prerelease {
		return "", errors.New("latest release has an invalid stable tag")
	}
	assets := make(map[string]bool, len(release.Assets))
	for _, asset := range release.Assets {
		assets[asset.Name] = true
	}
	for _, name := range []string{"checksums.txt", assetName("cli"), assetName("core")} {
		if !assets[name] {
			return "", fmt.Errorf("latest release is missing %s", name)
		}
	}
	return release.Tag, nil
}

func (s source) get(ctx context.Context, address string, limit int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || req.URL.Scheme != "https" || req.URL.User != nil {
		return nil, errors.New("release URL must use HTTPS")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// HTTP errors can include a redirected download URL and its query.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("release server could not be reached")
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength > limit {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("release server returned HTTP %d", resp.StatusCode)
		}
		return nil, errors.New("release response is too large")
	}
	return resp.Body, nil
}

func (s source) assetURL(tag, name string) string {
	return s.downloads + "/download/" + url.PathEscape(tag) + "/" + name
}

func (s source) checksums(ctx context.Context, tag string) (map[string][sha256.Size]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	body, err := s.get(ctx, s.assetURL(tag, "checksums.txt"), metadataLimit)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, metadataLimit+1))
	if err != nil || len(data) > metadataLimit {
		return nil, errors.New("could not read checksums.txt")
	}
	want := make(map[string][sha256.Size]byte, 2)
	cli, core := assetName("cli"), assetName("core")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != cli && name != core {
			continue
		}
		hash, err := hex.DecodeString(fields[0])
		_, duplicate := want[name]
		if len(fields) != 2 || err != nil || len(hash) != sha256.Size || duplicate {
			return nil, fmt.Errorf("invalid checksum for %s", name)
		}
		want[name] = [sha256.Size]byte(hash)
	}
	if len(want) != 2 {
		return nil, errors.New("checksums.txt is missing a checksum for this platform")
	}
	return want, nil
}

func (s source) download(ctx context.Context, tag, name, path string, want [sha256.Size]byte, mode os.FileMode) error {
	body, err := s.get(ctx, s.assetURL(tag, name), binaryLimit)
	if err != nil {
		return err
	}
	defer body.Close()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, binaryLimit+1))
	if err != nil {
		return err
	}
	if size == 0 || size > binaryLimit || [sha256.Size]byte(hash.Sum(nil)) != want {
		return fmt.Errorf("checksum or size mismatch for %s", name)
	}
	return errors.Join(file.Chmod(mode), file.Sync(), file.Close())
}

func (s source) cachedLatest(ctx context.Context) (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return s.latest(ctx)
	}
	dir := filepath.Join(root, "knot")
	path := filepath.Join(dir, "latest-release")
	if file, err := os.Open(path); err == nil {
		info, statErr := file.Stat()
		data, readErr := io.ReadAll(io.LimitReader(file, 256))
		file.Close()
		tag := string(data)
		if statErr == nil && readErr == nil && info.ModTime().Before(time.Now()) &&
			time.Since(info.ModTime()) < 30*time.Minute && validRelease(tag) {
			return tag, nil
		}
	}
	tag, err := s.latest(ctx)
	if err == nil && os.MkdirAll(dir, 0700) == nil {
		if file, createErr := os.CreateTemp(dir, ".latest-release-*"); createErr == nil {
			defer os.Remove(file.Name())
			_, writeErr := file.WriteString(tag)
			if closeErr := file.Close(); writeErr == nil && closeErr == nil {
				_ = os.Rename(file.Name(), path)
			}
		}
	}
	return tag, err
}
