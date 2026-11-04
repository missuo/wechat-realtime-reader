// Package config loads the private source mapping and database keys from
// ~/.config/wechat-realtime-reader. Key material never leaves this package
// except as raw bytes handed to the crypto layer; nothing here logs keys.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	keysFile   = "keys.json"
	sourceFile = "source-config.json"
)

// Source is the explicit account binding (source-config.json).
type Source struct {
	Wxid         string `json:"wxid"`
	DBBasePath   string `json:"db_base_path"`
	VaultDir     string `json:"vault_dir"`
	DecryptedDir string `json:"decrypted_dir"`
	ExportsDir   string `json:"exports_dir"`
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Dir    string // ~/.config/wechat-realtime-reader
	Source Source
	// Keys maps a db path relative to db_storage (e.g. "message/message_0.db")
	// to its hex key.
	Keys map[string]string
}

// DefaultDir returns the private config directory.
func DefaultDir() string {
	if d := os.Getenv("WECHAT_READER_CONFIG"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "wechat-realtime-reader")
}

// Load reads and validates the private config directory.
func Load(dir string) (*Config, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := checkPrivate(dir, true); err != nil {
		return nil, err
	}
	c := &Config{Dir: dir}
	if err := readPrivateJSON(filepath.Join(dir, sourceFile), &c.Source); err != nil {
		return nil, err
	}
	c.Source.DBBasePath = expand(c.Source.DBBasePath)
	c.Source.VaultDir = expand(c.Source.VaultDir)
	c.Source.DecryptedDir = expand(c.Source.DecryptedDir)
	c.Source.ExportsDir = expand(c.Source.ExportsDir)
	if c.Source.DBBasePath == "" {
		return nil, errors.New("source-config.json: db_base_path is required")
	}
	if c.Source.VaultDir == "" {
		return nil, errors.New("source-config.json: vault_dir is required")
	}
	if err := rejectUnsafeVault(c.Source.VaultDir); err != nil {
		return nil, err
	}

	raw := map[string]string{}
	if err := readPrivateJSON(filepath.Join(dir, keysFile), &raw); err != nil {
		return nil, err
	}
	c.Keys = make(map[string]string, len(raw))
	for name, k := range raw {
		c.Keys[NormalizeDBName(name)] = strings.TrimSpace(k)
	}
	return c, nil
}

// SelfWxid is the account's own username. Account directories are named
// "<wxid>_<4 hex>"; the suffix is not part of the username.
func (c *Config) SelfWxid() string {
	w := c.Source.Wxid
	if m := acctSuffix.FindStringSubmatch(w); m != nil {
		return m[1]
	}
	return w
}

var acctSuffix = regexp.MustCompile(`^(wxid_[0-9a-z]+)_[0-9a-f]{4}$`)

// SnapshotDir is where plaintext snapshots are published.
func (c *Config) SnapshotDir() string {
	if c.Source.DecryptedDir != "" {
		return c.Source.DecryptedDir
	}
	return filepath.Join(c.Source.VaultDir, "decrypted", "current")
}

// StateDir holds runtime state (lock, manifest, web token).
func (c *Config) StateDir() string { return filepath.Join(c.Source.VaultDir, "state") }

// NormalizeDBName maps the short names used in keys.json to the relative
// db path: "message_0" → "message/message_0.db", "contact" →
// "contact/contact.db"; paths containing "/" are kept as-is.
func NormalizeDBName(name string) string {
	name = strings.TrimSpace(name)
	if strings.Contains(name, "/") {
		if !strings.HasSuffix(name, ".db") {
			name += ".db"
		}
		return name
	}
	name = strings.TrimSuffix(name, ".db")
	for _, p := range []string{"message_", "biz_message_", "media_"} {
		if strings.HasPrefix(name, p) {
			return "message/" + name + ".db"
		}
	}
	return name + "/" + name + ".db"
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func readPrivateJSON(path string, v any) error {
	if err := checkPrivate(path, false); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		// Never echo file contents: they may contain keys.
		return fmt.Errorf("%s: invalid JSON", filepath.Base(path))
	}
	return nil
}

// checkPrivate refuses group/world-accessible secrets.
func checkPrivate(path string, dir bool) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if dir != st.IsDir() {
		return fmt.Errorf("%s: unexpected file type", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: permissions %o are too open (want 0%o)", path, st.Mode().Perm(), map[bool]os.FileMode{true: 0o700, false: 0o600}[dir])
	}
	return nil
}

// rejectUnsafeVault keeps plaintext out of synced or versioned places.
func rejectUnsafeVault(p string) error {
	home, _ := os.UserHomeDir()
	abs, _ := filepath.Abs(p)
	for _, bad := range []string{"Projects", "Desktop", "Downloads", "Documents", "Library/Mobile Documents", "Dropbox"} {
		if strings.HasPrefix(abs, filepath.Join(home, bad)+string(filepath.Separator)) {
			return fmt.Errorf("vault_dir must not live under ~/%s", bad)
		}
	}
	for d := abs; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return errors.New("vault_dir must not be inside a git repository")
		}
	}
	return nil
}

// AccountDir is xwechat_files/<account>, the parent of db_storage.
func (c *Config) AccountDir() string { return filepath.Dir(c.Source.DBBasePath) }

// ImageKeyPath stores the derived image (.dat) keys, 0600.
func (c *Config) ImageKeyPath() string { return filepath.Join(c.Dir, "image-key.json") }

// CacheDir holds converted media (plaintext, private).
func (c *Config) CacheDir() string { return filepath.Join(c.Source.VaultDir, "cache") }
