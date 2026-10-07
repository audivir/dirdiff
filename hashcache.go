package main

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RACY_WINDOW is how long after hashing a file must not have changed for its hash to be
// trusted, since a change within the timestamp resolution of the file system keeps the time.
const RACY_WINDOW = 2 * time.Second

// stamp stores the file attributes that change when its content changes.
type stamp struct {
	Size       int64
	ModTime    int64
	ChangeTime int64
	Inode      uint64
}

// cacheEntry stores a hash and the stamp of the file when it was hashed.
type cacheEntry struct {
	Stamp  stamp
	Hash   string
	Hashed int64
}

// hashCache manages the stored hashes of the files below one root directory.
type hashCache struct {
	mu      sync.Mutex
	file    string
	entries map[string]cacheEntry
	dirty   bool
}

var (
	hashCachesMu sync.Mutex
	hashCaches   = map[string]*hashCache{}
)

// cacheFor returns the hash cache of root, loading it from the user cache on first use.
func cacheFor(root string) *hashCache {
	hashCachesMu.Lock()
	defer hashCachesMu.Unlock()
	if c, ok := hashCaches[root]; ok {
		return c
	}
	c := &hashCache{entries: map[string]cacheEntry{}}
	if cacheDir, err := os.UserCacheDir(); err == nil {
		sum := sha256.Sum256([]byte(root))
		c.file = filepath.Join(cacheDir, BIN_NAME, "hashes", hex.EncodeToString(sum[:8])+".gob")
		if f, err := os.Open(c.file); err == nil {
			_ = gob.NewDecoder(f).Decode(&c.entries)
			_ = f.Close()
		}
	}
	hashCaches[root] = c
	return c
}

// cacheKey returns the key of a file hashed with a sparse limit.
func cacheKey(relPath string, limit int64) string {
	return relPath + "\x00" + strconv.FormatInt(limit, 10)
}

// lookup returns the stored hash if the file still has the stamp it had when hashed, and it
// had not changed shortly before.
func (c *hashCache) lookup(key string, st stamp) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.Stamp != st || st.ModTime+int64(RACY_WINDOW) >= e.Hashed {
		return "", false
	}
	return e.Hash, true
}

// store records the hash of a file with the stamp it had before hashing.
func (c *hashCache) store(key string, st stamp, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{Stamp: st, Hash: hash, Hashed: time.Now().UnixNano()}
	c.dirty = true
}

// prune drops the entries of files that no longer exist below root.
func (c *hashCache) prune(root string) {
	for key := range c.entries {
		relPath, _, _ := strings.Cut(key, "\x00")
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relPath))); os.IsNotExist(err) {
			delete(c.entries, key)
		}
	}
}

// saveHashCaches writes all changed caches, replacing each file atomically.
func saveHashCaches() {
	hashCachesMu.Lock()
	defer hashCachesMu.Unlock()
	for root, c := range hashCaches {
		c.mu.Lock()
		if c.dirty && c.file != "" {
			c.prune(root)
			if err := os.MkdirAll(filepath.Dir(c.file), 0o700); err == nil {
				if tmp, err := os.CreateTemp(filepath.Dir(c.file), "hashes.*"); err == nil {
					err := gob.NewEncoder(tmp).Encode(c.entries)
					if closeErr := tmp.Close(); err == nil && closeErr == nil {
						_ = os.Rename(tmp.Name(), c.file)
					}
					_ = os.Remove(tmp.Name())
				}
			}
			c.dirty = false
		}
		c.mu.Unlock()
	}
}
