package sftp

import (
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	remoteDirCacheTTL        = 2 * time.Second
	remoteDirCacheMaxEntries = 256
	remoteDirCacheMaxCost    = 4 << 20
)

type remoteDirReader interface {
	ReadDir(string) ([]os.FileInfo, error)
}
type remoteDirCacheEntry struct {
	files    []os.FileInfo
	loadedAt time.Time
	cost     int
}
type remoteDirCache struct {
	reader     remoteDirReader
	ttl        time.Duration
	now        func() time.Time
	mu         sync.Mutex
	sf         singleflight.Group
	entries    map[string]remoteDirCacheEntry
	cost       int
	generation uint64
	closed     bool
}

func newRemoteDirCache(reader remoteDirReader, ttl time.Duration) *remoteDirCache {
	if ttl <= 0 {
		ttl = remoteDirCacheTTL
	}
	return &remoteDirCache{reader: reader, ttl: ttl, now: time.Now, entries: make(map[string]remoteDirCacheEntry)}
}
func (c *remoteDirCache) ReadDir(name string) ([]os.FileInfo, error) {
	if c == nil || c.reader == nil {
		return nil, os.ErrInvalid
	}
	name = normalizeRemoteDir(name)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, os.ErrClosed
	}
	if entry, ok := c.entries[name]; ok && c.now().Sub(entry.loadedAt) < c.ttl {
		files := cloneFileInfos(entry.files)
		c.mu.Unlock()
		return files, nil
	}
	generation := c.generation
	c.mu.Unlock()
	// A read started after invalidation must not join a stale in-flight read.
	key := fmt.Sprintf("%d:%s", generation, name)
	res, err, _ := c.sf.Do(key, func() (any, error) {
		files, err := c.reader.ReadDir(name)
		if err != nil {
			return nil, err
		}
		cost := len(name) + 128
		for _, info := range files {
			cost += 128 + len(info.Name())
		}
		cloned := cloneFileInfos(files)
		c.mu.Lock()
		defer c.mu.Unlock()
		now := c.now()
		for key, entry := range c.entries {
			if now.Sub(entry.loadedAt) >= c.ttl {
				c.removeLocked(key)
			}
		}
		if c.closed || c.generation != generation || cost > remoteDirCacheMaxCost {
			return cloned, nil
		}
		c.removeLocked(name)
		for len(c.entries) >= remoteDirCacheMaxEntries || c.cost+cost > remoteDirCacheMaxCost {
			oldest := ""
			for key, entry := range c.entries {
				if oldest == "" || entry.loadedAt.Before(c.entries[oldest].loadedAt) || (entry.loadedAt.Equal(c.entries[oldest].loadedAt) && key < oldest) {
					oldest = key
				}
			}
			c.removeLocked(oldest)
		}
		c.entries[name] = remoteDirCacheEntry{files: cloned, loadedAt: now, cost: cost}
		c.cost += cost
		return cloned, nil
	})
	if err != nil {
		return nil, err
	}
	return cloneFileInfos(res.([]os.FileInfo)), nil
}
func (c *remoteDirCache) removeLocked(name string) {
	if entry, ok := c.entries[name]; ok {
		c.cost -= entry.cost
		delete(c.entries, name)
	}
}
func (c *remoteDirCache) Invalidate(paths ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// One bounded generation deliberately invalidates all in-flight fills;
	// unaffected resident entries remain usable. No per-path tombstones accumulate.
	c.generation++
	for _, p := range paths {
		if p != "" {
			c.removeLocked(normalizeRemoteDir(p))
		}
	}
}
func (c *remoteDirCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.generation++
	clear(c.entries)
	c.cost = 0
}
func cloneFileInfos(files []os.FileInfo) []os.FileInfo {
	if len(files) == 0 {
		return nil
	}
	return append([]os.FileInfo(nil), files...)
}
func normalizeRemoteDir(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean(p)
}
