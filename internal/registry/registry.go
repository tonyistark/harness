// Package registry manages the local skill registry: GPN manifests on top of
// Claude Code's plugin layout, a canonical content digest, ed25519 signing
// (cosign/Sigstore at GA), tier checks and verified install.
package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/gpn/gpn/internal/store"
)

// Manifest is gpn.manifest.yaml inside a plugin directory.
type Manifest struct {
	Name         string   `yaml:"name"`
	Version      string   `yaml:"version"`
	Owner        string   `yaml:"owner"`
	Tier         string   `yaml:"tier"` // official | enterprise | custom
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed_tools"`
	DataClass    string   `yaml:"data_class"`
	ModelPolicy  []string `yaml:"model_policy"` // tiers allowed to run it
	Evals        Evals    `yaml:"evals"`
	Digest       string   `yaml:"digest"`
}

// Evals configures the eval gate for a plugin.
type Evals struct {
	Dir        string  `yaml:"dir"`
	Threshold  float64 `yaml:"threshold"`
	JudgeModel string  `yaml:"judge_model"`
	Runs       int     `yaml:"runs"`
}

// Signature is gpn.manifest.sig.
type Signature struct {
	Alg       string `json:"alg"`
	KeyID     string `json:"key_id"`
	KeyFP     string `json:"key_fingerprint"`
	Digest    string `json:"digest"`
	Sig       string `json:"sig"`
	SignedAt  string `json:"signed_at"`
	Signer    string `json:"signer"`
	ManifestS string `json:"manifest_sha256"`
}

const (
	manifestFile  = "gpn.manifest.yaml"
	signatureFile = "gpn.manifest.sig"
)

// LoadManifest reads a plugin's manifest.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Name == "" || m.Version == "" || m.Tier == "" || m.Owner == "" {
		return nil, fmt.Errorf("manifest: name, version, tier and owner are required")
	}
	return &m, nil
}

// Digest computes the canonical SHA-256 over every file in the plugin
// directory except the signature file and the manifest's own digest line:
// paths sorted, each contributes "path\n" + size + "\n" + content hash.
func Digest(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "results" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == signatureFile {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		if rel == manifestFile {
			b = stripDigestLine(b)
		}
		fh := sha256.Sum256(b)
		fmt.Fprintf(h, "%s\n%d\n%x\n", filepath.ToSlash(rel), len(b), fh)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func stripDigestLine(b []byte) []byte {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "digest:") {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// Keys is a local ed25519 key pair; the prototype's stand-in for Sigstore.
type Keys struct {
	ID   string
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// Fingerprint is the SHA-256 of the public key, used to tell apart two key
// pairs that share an id (for example after an accidental keygen).
func (k *Keys) Fingerprint() string {
	sum := sha256.Sum256(k.Pub)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// KeysExist reports whether a key pair with this id is already on disk.
func KeysExist(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, id+".pub"))
	return err == nil
}

// GenerateKeys writes a new key pair under dir and returns it. It refuses to
// overwrite an existing pair; delete it deliberately first (every plugin it
// signed will need republishing).
func GenerateKeys(dir, id string) (*Keys, error) {
	if KeysExist(dir, id) {
		return nil, fmt.Errorf("key %q already exists in %s; remove it first if you mean to rotate (plugins it signed must be republished)", id, dir)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".key"), []byte(hex.EncodeToString(priv)), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".pub"), []byte(hex.EncodeToString(pub)), 0o644); err != nil {
		return nil, err
	}
	return &Keys{ID: id, Priv: priv, Pub: pub}, nil
}

// LoadKeys reads a key pair (the private half is optional for verification).
func LoadKeys(dir, id string) (*Keys, error) {
	k := &Keys{ID: id}
	pubHex, err := os.ReadFile(filepath.Join(dir, id+".pub"))
	if err != nil {
		return nil, fmt.Errorf("public key %s: %w", id, err)
	}
	pub, err := hex.DecodeString(strings.TrimSpace(string(pubHex)))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key %s: malformed", id)
	}
	k.Pub = ed25519.PublicKey(pub)
	if privHex, err := os.ReadFile(filepath.Join(dir, id+".key")); err == nil {
		priv, err := hex.DecodeString(strings.TrimSpace(string(privHex)))
		if err == nil && len(priv) == ed25519.PrivateKeySize {
			k.Priv = ed25519.PrivateKey(priv)
		}
	}
	return k, nil
}

// Publish computes the digest, writes it into the manifest, and signs.
func Publish(dir string, k *Keys, signer string) (*Manifest, *Signature, error) {
	if k == nil || k.Priv == nil {
		return nil, nil, fmt.Errorf("publish needs a private key")
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, nil, err
	}
	// Normalize the manifest on disk first so the digest covers the exact
	// bytes a verifier will read (minus the digest line itself).
	m.Digest = ""
	norm, err := yaml.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), norm, 0o644); err != nil {
		return nil, nil, err
	}
	d, err := Digest(dir)
	if err != nil {
		return nil, nil, err
	}
	m.Digest = d
	mb, err := yaml.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), mb, 0o644); err != nil {
		return nil, nil, err
	}
	if d2, err := Digest(dir); err != nil || d2 != d {
		return nil, nil, fmt.Errorf("digest unstable after writing manifest (%s vs %s)", d, d2)
	}
	msum := sha256.Sum256(stripDigestLine(mb))
	sig := &Signature{Alg: "ed25519", KeyID: k.ID, KeyFP: k.Fingerprint(), Digest: d, SignedAt: time.Now().UTC().Format(time.RFC3339), Signer: signer, ManifestS: hex.EncodeToString(msum[:])}
	sig.Sig = hex.EncodeToString(ed25519.Sign(k.Priv, []byte(signingPayload(sig))))
	sb, _ := json.MarshalIndent(sig, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, signatureFile), sb, 0o644); err != nil {
		return nil, nil, err
	}
	return m, sig, nil
}

func signingPayload(s *Signature) string {
	return s.Alg + "|" + s.KeyID + "|" + s.Digest + "|" + s.ManifestS + "|" + s.SignedAt
}

// Verify recomputes the digest and checks the signature with the given key.
// It returns the manifest when everything matches.
func Verify(dir string, k *Keys) (*Manifest, *Signature, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, nil, err
	}
	sb, err := os.ReadFile(filepath.Join(dir, signatureFile))
	if err != nil {
		return nil, nil, fmt.Errorf("unsigned plugin: %w", err)
	}
	var sig Signature
	if err := json.Unmarshal(sb, &sig); err != nil {
		return nil, nil, fmt.Errorf("signature: %w", err)
	}
	d, err := Digest(dir)
	if err != nil {
		return nil, nil, err
	}
	if d != sig.Digest || d != m.Digest {
		return nil, nil, fmt.Errorf("digest mismatch: content %s, signature %s, manifest %s", d, sig.Digest, m.Digest)
	}
	if k == nil || sig.KeyID != k.ID {
		return nil, nil, fmt.Errorf("signature key %q is not trusted", sig.KeyID)
	}
	if sig.KeyFP != "" && sig.KeyFP != k.Fingerprint() {
		return nil, nil, fmt.Errorf("signed with key %q %s but the local key %q is %s: the key was regenerated; republish the plugin (gpn registry publish) and reinstall", sig.KeyID, sig.KeyFP, k.ID, k.Fingerprint())
	}
	mb, _ := os.ReadFile(filepath.Join(dir, manifestFile))
	msum := sha256.Sum256(stripDigestLine(mb))
	if hex.EncodeToString(msum[:]) != sig.ManifestS {
		return nil, nil, fmt.Errorf("manifest altered after signing")
	}
	raw, err := hex.DecodeString(sig.Sig)
	if err != nil || !ed25519.Verify(k.Pub, []byte(signingPayload(&sig)), raw) {
		return nil, nil, fmt.Errorf("signature invalid")
	}
	return m, &sig, nil
}

// Install verifies a plugin directory, checks its tier against the allowed
// list and records the install. The returned path is what the driver passes
// with --plugin-dir.
func Install(st *store.Store, dir string, k *Keys, tiersAllowed []string, requireSig bool) (*Manifest, error) {
	var m *Manifest
	var sig *Signature
	var err error
	if requireSig {
		m, sig, err = Verify(dir, k)
		if err != nil {
			return nil, err
		}
	} else {
		m, err = LoadManifest(dir)
		if err != nil {
			return nil, err
		}
	}
	allowed := false
	for _, t := range tiersAllowed {
		if t == m.Tier {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("plugin %s is tier %q; policy allows %v", m.Name, m.Tier, tiersAllowed)
	}
	abs, _ := filepath.Abs(dir)
	sk := store.Skill{Name: m.Name, Version: m.Version, Tier: m.Tier, Digest: m.Digest, Owner: m.Owner, Path: abs}
	if sig != nil {
		sk.Signature = sig.Sig
	}
	if err := st.UpsertSkill(sk); err != nil {
		return nil, err
	}
	return m, nil
}
