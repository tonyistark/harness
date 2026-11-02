// Package assets embeds the seed files `gpn init` writes into a repository:
// the example policy and model catalog, workflows, schemas, prompts, the
// constitution template and the sdd-core plugin.
package assets

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:seed
var seed embed.FS

// Seed returns the embedded seed tree rooted at "seed".
func Seed() fs.FS {
	sub, _ := fs.Sub(seed, "seed")
	return sub
}

// Extract copies one embedded subtree (e.g. "registry/sdd-core") into dst.
// Existing files are left alone unless force is set.
func Extract(src, dst string, force bool) error {
	root := Seed()
	return fs.WalkDir(root, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !force {
			if _, err := os.Stat(target); err == nil {
				return nil
			}
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// Read returns one embedded file.
func Read(p string) ([]byte, error) { return fs.ReadFile(Seed(), p) }
