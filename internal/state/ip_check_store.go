package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
)

// ipCheckRecord is an immutable journal value, separate from the caller's
// mutable source result. IP quality data never joins a proxy-path reader.
type ipCheckRecord struct {
	Key         ipCheckKey
	ResultJSON  string
	CheckedAtNs int64
	ExpiresAtNs int64
}

type ipCheckKey struct {
	SubjectKind string
	Subject     string
	Source      string
}

const maxIPCheckResultBytes = 256 * 1024

type ipCheckStore struct{ engine *StateEngine }

var _ ipcheck.Store = (*ipCheckStore)(nil)

// IPCheckStore returns a restricted cold-path cache capability. Saves and
// deletes enter the same weak-persistence lifecycle as other cache.db data;
// CacheFlushWorker.Stop flushes them before the database closes.
func (e *StateEngine) IPCheckStore() ipcheck.Store {
	return &ipCheckStore{engine: e}
}

func validateIPCheckKey(key ipCheckKey) error {
	if strings.TrimSpace(key.Source) == "" || len(key.Source) > 128 {
		return errors.New("IP quality cache: invalid source")
	}
	switch key.SubjectKind {
	case ipcheck.SubjectIP:
		ip, err := netip.ParseAddr(key.Subject)
		if err != nil || ip.Zone() != "" || ip.Unmap().String() != key.Subject {
			return errors.New("IP quality cache: invalid IP subject")
		}
	case ipcheck.SubjectNode:
		hash, err := node.ParseHex(key.Subject)
		if err != nil || hash.Hex() != key.Subject {
			return errors.New("IP quality cache: invalid node subject")
		}
	default:
		return errors.New("IP quality cache: invalid subject kind")
	}
	return nil
}

func (s *ipCheckStore) Load(ctx context.Context, subjectKind, subject, source string) (*ipcheck.SourceResult, error) {
	key := ipCheckKey{SubjectKind: subjectKind, Subject: subject, Source: source}
	if err := validateIPCheckKey(key); err != nil {
		return nil, err
	}
	e := s.engine
	e.ipCheckMu.Lock()
	defer e.ipCheckMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var record *ipCheckRecord
	if pending, ok := e.ipCheckPending[key]; ok {
		if pending == nil {
			return nil, nil
		}
		record = pending
	} else {
		if _, deleted := e.ipCheckDeletedNodes[subject]; subjectKind == ipcheck.SubjectNode && deleted {
			return nil, nil
		}
		record = &ipCheckRecord{Key: key}
		err := e.CacheRepo.db.QueryRowContext(ctx, `SELECT result_json, checked_at_ns, expires_at_ns
			FROM ip_check_results WHERE subject_kind = ? AND subject = ? AND source = ?`,
			subjectKind, subject, source).Scan(&record.ResultJSON, &record.CheckedAtNs, &record.ExpiresAtNs)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load IP quality cache: %w", err)
		}
	}
	if record.ExpiresAtNs <= time.Now().UnixNano() {
		// Expiry is enforced on every read and reclaimed on the next batch.
		e.ipCheckPending[key] = nil
		return nil, nil
	}
	if len(record.ResultJSON) > maxIPCheckResultBytes {
		return nil, errors.New("IP quality cache: result too large")
	}
	var result ipcheck.SourceResult
	if err := json.Unmarshal([]byte(record.ResultJSON), &result); err != nil {
		return nil, fmt.Errorf("decode IP quality cache: %w", err)
	}
	if result.Source != source || result.CheckedAt.UnixNano() != record.CheckedAtNs || result.ExpiresAt.UnixNano() != record.ExpiresAtNs {
		return nil, errors.New("IP quality cache: inconsistent stored result")
	}
	return &result, nil
}

func (s *ipCheckStore) Save(ctx context.Context, subjectKind, subject string, result *ipcheck.SourceResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if result == nil {
		return errors.New("IP quality cache: nil result")
	}
	key := ipCheckKey{SubjectKind: subjectKind, Subject: subject, Source: result.Source}
	if err := validateIPCheckKey(key); err != nil {
		return err
	}
	if result.CheckedAt.IsZero() || !result.ExpiresAt.After(result.CheckedAt) {
		return errors.New("IP quality cache: invalid result lifetime")
	}
	if (subjectKind == ipcheck.SubjectIP && result.Kind != ipcheck.SourceKindByIP) ||
		(subjectKind == ipcheck.SubjectNode && result.Kind != ipcheck.SourceKindViaNode) {
		return errors.New("IP quality cache: source kind does not match subject")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode IP quality cache: %w", err)
	}
	if len(data) > maxIPCheckResultBytes {
		return errors.New("IP quality cache: result too large")
	}
	record := &ipCheckRecord{Key: key, ResultJSON: string(data), CheckedAtNs: result.CheckedAt.UnixNano(), ExpiresAtNs: result.ExpiresAt.UnixNano()}
	e := s.engine
	e.ipCheckMu.Lock()
	defer e.ipCheckMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if current := e.ipCheckPending[key]; current != nil && current.CheckedAtNs > record.CheckedAtNs {
		return nil
	}
	e.ipCheckPending[key] = record
	return nil
}

func (s *ipCheckStore) DeleteNode(ctx context.Context, hash node.Hash) error {
	e := s.engine
	e.ipCheckMu.Lock()
	defer e.ipCheckMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	subject := hash.Hex()
	for key := range e.ipCheckPending {
		if key.SubjectKind == ipcheck.SubjectNode && key.Subject == subject {
			delete(e.ipCheckPending, key)
		}
	}
	e.ipCheckDeletedNodes[subject] = struct{}{}
	return nil
}

func (e *StateEngine) ipCheckDirtyCount() int {
	e.ipCheckMu.Lock()
	defer e.ipCheckMu.Unlock()
	return len(e.ipCheckPending) + len(e.ipCheckDeletedNodes)
}

// pendingIPCheckOps must be called with ipCheckMu held. The pending journal is
// only cleared after the surrounding cache transaction commits successfully.
func (e *StateEngine) pendingIPCheckOps() (upserts []ipCheckRecord, deletes []ipCheckKey, deletedNodes []string) {
	for key, value := range e.ipCheckPending {
		if value == nil {
			deletes = append(deletes, key)
		} else {
			upserts = append(upserts, *value)
		}
	}
	for subject := range e.ipCheckDeletedNodes {
		deletedNodes = append(deletedNodes, subject)
	}
	return
}

func flushIPCheckOps(tx *sql.Tx, ops FlushOps) error {
	// Subject-wide invalidations precede any newer per-source saves.
	for _, subject := range ops.deleteIPCheckNodes {
		if _, err := tx.Exec(`DELETE FROM ip_check_results WHERE subject_kind = ? AND subject = ?`, ipcheck.SubjectNode, subject); err != nil {
			return fmt.Errorf("delete node IP quality cache: %w", err)
		}
	}
	if err := bulkExecTx(tx, `DELETE FROM ip_check_results WHERE subject_kind = ? AND subject = ? AND source = ?`, len(ops.deleteIPChecks), func(stmt *sql.Stmt, i int) error {
		key := ops.deleteIPChecks[i]
		_, err := stmt.Exec(key.SubjectKind, key.Subject, key.Source)
		return err
	}); err != nil {
		return fmt.Errorf("delete IP quality cache: %w", err)
	}
	if err := bulkExecTx(tx, `INSERT INTO ip_check_results
		(subject_kind, subject, source, result_json, checked_at_ns, expires_at_ns)
		SELECT ?, ?, ?, ?, ?, ? WHERE ? != ? OR EXISTS (SELECT 1 FROM nodes_static WHERE hash = ?)
		ON CONFLICT(subject_kind, subject, source) DO UPDATE SET
		result_json = excluded.result_json, checked_at_ns = excluded.checked_at_ns, expires_at_ns = excluded.expires_at_ns
		WHERE excluded.checked_at_ns >= ip_check_results.checked_at_ns`, len(ops.upsertIPChecks), func(stmt *sql.Stmt, i int) error {
		row := ops.upsertIPChecks[i]
		_, err := stmt.Exec(row.Key.SubjectKind, row.Key.Subject, row.Key.Source, row.ResultJSON, row.CheckedAtNs, row.ExpiresAtNs,
			row.Key.SubjectKind, ipcheck.SubjectNode, row.Key.Subject)
		return err
	}); err != nil {
		return fmt.Errorf("upsert IP quality cache: %w", err)
	}
	if ops.pruneIPChecksBefore > 0 {
		if _, err := tx.Exec(`DELETE FROM ip_check_results WHERE expires_at_ns <= ?`, ops.pruneIPChecksBefore); err != nil {
			return fmt.Errorf("expire IP quality cache: %w", err)
		}
	}
	return nil
}
