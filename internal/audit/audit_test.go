package audit

import (
	"path/filepath"
	"testing"

	"github.com/gpn/gpn/internal/store"
)

func TestChainVerifyAndTamper(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "gpn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := &Chain{S: st}
	for i := 0; i < 3; i++ {
		if _, err := c.Append("RUN-1", "s", "test", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := c.Verify(); err != nil || n != 3 {
		t.Fatalf("verify: %d %v", n, err)
	}
	if _, err := st.DB.Exec(`UPDATE audit SET payload_json='{"i":99}' WHERE seq=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(); err == nil {
		t.Fatal("tampered chain verified")
	}
}
