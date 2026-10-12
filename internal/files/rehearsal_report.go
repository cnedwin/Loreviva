package files

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
)

type rehearsalReportEvidence struct {
	owner           *Store
	root            domain.TaskLease
	worker          string
	children        []store.RehearsalReportChild
	context         context.Context
	release         func()
	closed          atomic.Bool
	once            sync.Once
	artifact        store.RehearsalReportArtifact
	artifactRelease func()
}

func (e *rehearsalReportEvidence) Close() {
	e.once.Do(func() {
		e.closed.Store(true)
		if e.artifactRelease != nil {
			e.artifactRelease()
		}
		e.release()
	})
}

// All selected creative sources precede all child body locks. Frozen input
// order and original0->1 order are fixed; Close retains them through a caller's
// actual owning transaction and releases them in reverse order on every exit.
func (f *Store) ReadRehearsalReportEvidence(ctx context.Context, root domain.TaskLease, worker string) (store.RehearsalReportEvidence, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	ctx, n, close, e := f.DB.EnterRehearsalReportRead(ctx, root, worker)
	if e != nil {
		return nil, e
	}
	releases := []func(){close}
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	out := &rehearsalReportEvidence{owner: f, root: root, worker: worker, context: ctx}
	fail := func(e error) (store.RehearsalReportEvidence, error) { release(); return nil, e }
	keys := map[string]bool{}
	for index := 0; index < n; index++ {
		c, e := f.DB.InspectRehearsalChildOutcome(ctx, root, worker, index)
		if e != nil {
			return fail(e)
		}
		metas := []domain.MockResultMetadata{c.Metadata}
		if c.OriginalDependency != nil {
			metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
		}
		for _, m := range metas {
			if keys[m.BodyStorage] {
				return fail(ErrNotFound)
			}
			keys[m.BodyStorage] = true
			done, e := f.lockDerived(ctx, root.ProductScope, m.BodyStorage)
			if e != nil {
				return fail(e)
			}
			releases = append(releases, done)
		}
		before, e := f.DB.InspectRehearsalChildOutcome(ctx, root, worker, index)
		if e != nil {
			return fail(e)
		}
		if !reflect.DeepEqual(c, before) {
			return fail(ErrNotFound)
		}
		var body []byte
		for _, m := range metas {
			body, e = f.readLockedResultBytes(root.ProductScope, m)
			if e != nil {
				return fail(e)
			}
		}
		out.children = append(out.children, store.RehearsalReportChild{Index: index, Commit: c, Body: body})
	}
	// Check all children again while every original dependency lock is held.
	for _, child := range out.children {
		current, e := f.DB.InspectRehearsalChildOutcome(ctx, root, worker, child.Index)
		if e != nil {
			return fail(e)
		}
		if !reflect.DeepEqual(current, child.Commit) {
			return fail(ErrNotFound)
		}
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return fail(e)
	}
	out.release = release
	return out, nil
}

func (f *Store) RehearsalReportChildren(e store.RehearsalReportEvidence, root domain.TaskLease, worker string) ([]store.RehearsalReportChild, error) {
	held, ok := e.(*rehearsalReportEvidence)
	if !ok || held == nil || held.owner != f || held.closed.Load() || held.root != root || held.worker != worker {
		return nil, ErrNotFound
	}
	// Defensive deep copies include original dependency pointers, event IDs and
	// memory IDs. Mutating a returned report candidate cannot alter held proof.
	raw, err := json.Marshal(held.children)
	if err != nil {
		return nil, err
	}
	var out []store.RehearsalReportChild
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (f *Store) RehearsalReportContext(e store.RehearsalReportEvidence, root domain.TaskLease, worker string) (context.Context, error) {
	held, ok := e.(*rehearsalReportEvidence)
	if !ok || held == nil || held.owner != f || held.closed.Load() || held.root != root || held.worker != worker {
		return nil, ErrNotFound
	}
	return held.context, nil
}

func (f *Store) RehearsalReportArtifact(e store.RehearsalReportEvidence, root domain.TaskLease, worker string) (store.RehearsalReportArtifact, error) {
	held, ok := e.(*rehearsalReportEvidence)
	if !ok || held == nil || held.owner != f || held.closed.Load() || held.root != root || held.worker != worker || held.artifact.Storage == "" {
		return store.RehearsalReportArtifact{}, ErrNotFound
	}
	a := held.artifact
	a.Body = append([]byte(nil), a.Body...)
	return a, nil
}

func (f *Store) StageRehearsalReport(e store.RehearsalReportEvidence, root domain.TaskLease, worker string, body []byte) error {
	held, ok := e.(*rehearsalReportEvidence)
	if !ok || held == nil || held.owner != f || held.closed.Load() || held.root != root || held.worker != worker || held.artifact.Storage != "" {
		return ErrNotFound
	}
	ctx := held.context
	stage, x := f.AllocateDerived(root.ProductScope, string(body))
	if x != nil {
		return x
	}
	var old, hash string
	var live bool
	x = f.DB.DB.QueryRowContext(ctx, "SELECT storage_key,body_hash,delete_intent=0 FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=?", root.TenantID, root.ProductID, root.TaskID).Scan(&old, &hash, &live)
	existing := x == nil
	if x != nil && !errors.Is(x, sql.ErrNoRows) {
		return x
	}
	if existing {
		if !live || hash != stage.Hash {
			return ErrNotFound
		}
		stage, x = f.RecoverDerived(root.ProductScope, old, string(body))
		if x != nil {
			return x
		}
	}
	done, x := f.lockDerived(ctx, root.ProductScope, stage.key)
	if x != nil {
		return x
	}
	held.artifactRelease = done
	held.artifact = store.RehearsalReportArtifact{Storage: stage.key, Hash: stage.Hash, Body: append([]byte(nil), body...)}
	if !existing {
		// Fixed Store owner reconstructs the safe report and commits its exact source
		// binding before any bytes are written. An unknown binding ACK grants no IO.
		if x = f.DB.BindRehearsalReportStage(ctx, e, root, worker); x != nil {
			return x
		}
		file, x := f.root.OpenFile(stage.relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if x != nil {
			return x
		}
		_, x = file.Write(body)
		if x == nil {
			x = file.Sync()
		}
		closeErr := file.Close()
		if x != nil {
			return x
		}
		if closeErr != nil {
			return closeErr
		}
	}
	file, x := f.root.Open(stage.relative)
	if x != nil {
		return ErrNotFound
	}
	actual, x := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	file.Close()
	sum := sha256.Sum256(actual)
	if x != nil || hex.EncodeToString(sum[:]) != stage.Hash || len(actual) != len(body) {
		return ErrNotFound
	}
	// Current proof after physical IO. This remains a pure inspection, not a
	// business report or new generation capability.
	for _, child := range held.children {
		current, x := f.DB.InspectRehearsalChildOutcome(ctx, root, worker, child.Index)
		if x != nil {
			return x
		}
		if !reflect.DeepEqual(current, child.Commit) {
			return ErrNotFound
		}
	}
	return nil
}
