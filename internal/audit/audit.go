// Package audit keeps the append-only, hash-chained record of what each run
// did: prompts in force, models that answered, tool calls and decisions,
// approvals. Each row's hash covers the previous row's hash, so any edit or
// deletion is detectable with Verify.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gpn/gpn/internal/store"
)

// Chain appends to and verifies the audit table.
type Chain struct{ S *store.Store }

// Append records one entry. payload is any JSON-serializable value.
func (c *Chain) Append(runID, stepID, kind string, payload any) (store.AuditRow, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return store.AuditRow{}, err
	}
	last, err := c.S.LastAudit()
	if err != nil {
		return store.AuditRow{}, err
	}
	row := store.AuditRow{Seq: last.Seq + 1, Time: time.Now(), RunID: runID, StepID: stepID, Kind: kind, PayloadJSON: string(b), PayloadHash: sum(b), PrevHash: last.Hash}
	row.Hash = rowHash(row)
	return row, c.S.InsertAudit(row)
}

// Verify walks the chain and reports the first broken link.
func (c *Chain) Verify() (count int, err error) {
	rows, err := c.S.ListAudit()
	if err != nil {
		return 0, err
	}
	prev := ""
	for i, r := range rows {
		if r.Seq != int64(i+1) {
			return i, fmt.Errorf("audit: gap at seq %d (expected %d)", r.Seq, i+1)
		}
		if r.PrevHash != prev {
			return i, fmt.Errorf("audit: seq %d prev_hash mismatch", r.Seq)
		}
		if sum([]byte(r.PayloadJSON)) != r.PayloadHash {
			return i, fmt.Errorf("audit: seq %d payload altered", r.Seq)
		}
		if rowHash(r) != r.Hash {
			return i, fmt.Errorf("audit: seq %d hash mismatch", r.Seq)
		}
		prev = r.Hash
	}
	return len(rows), nil
}

func rowHash(r store.AuditRow) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s|%s", r.Seq, r.Time.UTC().Format(time.RFC3339Nano), r.RunID, r.StepID, r.Kind, r.PayloadHash, r.PrevHash)
	return hex.EncodeToString(h.Sum(nil))
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
