package maintenance

import (
	"context"
	"errors"
	"testing"
)

func TestPurgeContinuationRequiresOriginalLiveAdmission(t *testing.T) {
	ctx := context.Background()
	c, e := CreateCoordinator(t.TempDir(), "installation", "database", "files")
	if e != nil {
		t.Fatal(e)
	}
	p, e := c.Register(ctx, "files", []Writer{StoreWriter, FileWriter})
	if e != nil {
		t.Fatal(e)
	}
	other, e := c.Register(ctx, "other", []Writer{StoreWriter, FileWriter})
	if e != nil {
		t.Fatal(e)
	}
	admitted, release, e := p.Enter(ctx, StoreWriter)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if _, done, e := p.EnterRehearsalPurgeACK(admitted); e != nil {
		t.Fatal("OPEN original", e)
	} else {
		done()
	}
	if _, _, e = p.EnterRehearsalPurgeACK(ctx); !errors.Is(e, ErrMaintenance) {
		t.Fatal("no original admission", e)
	}
	if _, _, e = other.EnterRehearsalPurgeACK(admitted); !errors.Is(e, ErrMaintenance) {
		t.Fatal("wrong participant", e)
	}
	if e = c.BeginClose(ctx, "purge"); e != nil {
		t.Fatal(e)
	}
	if _, done, e := p.EnterRehearsalPurgeACK(admitted); e != nil {
		t.Fatal("CLOSING original", e)
	} else {
		done()
	}
	if _, _, e = p.Enter(admitted, StoreWriter); !errors.Is(e, ErrMaintenance) {
		t.Fatal("general nested entry widened", e)
	}
	a, e := c.load()
	if e != nil {
		t.Fatal(e)
	}
	original := a.Participants[p.id]
	changed := original
	changed.Incarnation = "wrong"
	a.Participants[p.id] = changed
	if e = c.save(a); e != nil {
		t.Fatal(e)
	}
	if _, _, e = p.EnterRehearsalPurgeACK(admitted); !errors.Is(e, ErrMaintenance) {
		t.Fatal("changed incarnation", e)
	}
	a.Participants[p.id] = original
	if e = c.save(a); e != nil {
		t.Fatal(e)
	}
	release()
	if _, _, e = p.EnterRehearsalPurgeACK(admitted); !errors.Is(e, ErrMaintenance) {
		t.Fatal("closed token", e)
	}
	if e = c.Drain(ctx, "purge"); e != nil {
		t.Fatal(e)
	}
	if _, _, e = p.EnterRehearsalPurgeACK(admitted); !errors.Is(e, ErrMaintenance) {
		t.Fatal("CLOSED", e)
	}
}
