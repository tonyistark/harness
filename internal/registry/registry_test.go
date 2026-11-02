package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gpn/gpn/internal/assets"
	"github.com/gpn/gpn/internal/store"
)

func TestPublishVerifyTamperInstall(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "sdd-core")
	if err := assets.Extract("registry/sdd-core", plugin, true); err != nil {
		t.Fatal(err)
	}
	keys, err := GenerateKeys(filepath.Join(dir, "keys"), "test")
	if err != nil {
		t.Fatal(err)
	}
	m, sig, err := Publish(plugin, keys, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest == "" || sig.Sig == "" {
		t.Fatal("publish produced no digest or signature")
	}
	if _, _, err := Verify(plugin, keys); err != nil {
		t.Fatalf("verify after publish: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "gpn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := Install(st, plugin, keys, []string{"official"}, true); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := Install(st, plugin, keys, []string{"custom"}, true); err == nil {
		t.Fatal("tier official should not install when only custom is allowed")
	}
	// Tamper with one byte of a skill.
	p := filepath.Join(plugin, "skills", "review", "SKILL.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, '!'), 0o644)
	if _, _, err := Verify(plugin, keys); err == nil {
		t.Fatal("tampered plugin verified")
	}
	if _, err := Install(st, plugin, keys, []string{"official"}, true); err == nil {
		t.Fatal("tampered plugin installed")
	}
	// Wrong key.
	other, _ := GenerateKeys(filepath.Join(dir, "keys2"), "other")
	os.WriteFile(p, b, 0o644)
	if _, _, err := Verify(plugin, other); err == nil {
		t.Fatal("verified with an untrusted key")
	}
}

func TestKeyRotationIsDetected(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "sdd-core")
	if err := assets.Extract("registry/sdd-core", plugin, true); err != nil {
		t.Fatal(err)
	}
	keysDir := filepath.Join(dir, "keys")
	k1, err := GenerateKeys(keysDir, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Publish(plugin, k1, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKeys(keysDir, "local"); err == nil {
		t.Fatal("keygen must refuse to overwrite an existing key")
	}
	// Simulate a deliberate rotation: remove and regenerate.
	os.Remove(filepath.Join(keysDir, "local.key"))
	os.Remove(filepath.Join(keysDir, "local.pub"))
	k2, _ := GenerateKeys(keysDir, "local")
	_, _, err = Verify(plugin, k2)
	if err == nil || !strings.Contains(err.Error(), "regenerated") {
		t.Fatalf("expected a key-regenerated error, got %v", err)
	}
	if _, _, err := Publish(plugin, k2, "t"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(plugin, k2); err != nil {
		t.Fatalf("republished plugin should verify: %v", err)
	}
}
