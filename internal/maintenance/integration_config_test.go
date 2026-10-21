package maintenance

import (
	"context"
	"testing"
	"time"
)

func TestIntegrationConfigurationWorldAccounting(t *testing.T) {
	writers := []Writer{APIWriter, StoreWriter, WorkerWriter, FileWriter, GovernanceWriter, HarnessWriter, MockWriter, QueryWriter, CleanupWriter, DeadlineWriter, ConfigWriter}
	for _, mode := range []string{"registered", "second-participant", "missing-config", "foreign-registration", "unknown-registration", "notifications", "released-proof", "drain"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			c, e := CreateCoordinator(t.TempDir(), "installation", "database", "files")
			if e != nil {
				t.Fatal(e)
			}
			own := append([]Writer(nil), writers...)
			if mode == "missing-config" {
				own = own[:len(own)-1]
			}
			p, e := c.Register(ctx, "api", own)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "second-participant" {
				other, err := c.Register(ctx, "worker", writers)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close(ctx)
			}
			if mode == "foreign-registration" || mode == "unknown-registration" || mode == "notifications" {
				w := ConfigWriter
				if mode == "notifications" {
					w = NotificationWriter
				}
				other, err := c.Register(ctx, "other", []Writer{w})
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close(ctx)
				if mode != "notifications" {
					unlock, err := c.root.Lock(ctx, "state.lock")
					if err != nil {
						t.Fatal(err)
					}
					state, err := c.load()
					if err != nil {
						unlock()
						t.Fatal(err)
					}
					r := state.Participants["other"]
					if mode == "foreign-registration" {
						r.Generation++
					} else {
						r.Writers = append(r.Writers, Writer("UNKNOWN"))
					}
					state.Participants["other"] = r
					err = c.save(state)
					unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "released-proof" || mode == "drain" {
				entered, identity, release, err := p.EnterConfiguration(ctx)
				if err != nil || identity.Installation != "installation" || identity.Generation != 1 {
					t.Fatal(identity, err)
				}
				if mode == "released-proof" {
					release()
					if _, _, done, err := p.EnterWorldAccounting(entered); err == nil {
						done()
						t.Fatal("released proof admitted")
					}
					return
				}
				if err = c.BeginClose(ctx, "configuration-close"); err != nil {
					release()
					t.Fatal(err)
				}
				if _, _, done, err := p.EnterConfiguration(ctx); err == nil {
					done()
					release()
					t.Fatal("CONFIG admitted after close")
				}
				short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
				err = c.Drain(short, "configuration-close")
				cancel()
				if err == nil {
					release()
					t.Fatal("drain ignored CONFIG permit")
				}
				release()
				if err = c.Drain(ctx, "configuration-close"); err != nil {
					t.Fatal(err)
				}
				if _, _, done, err := p.EnterWorldAccounting(ctx); err == nil {
					done()
					t.Fatal("closed accounting admitted")
				}
				return
			}
			_, id, done, err := p.EnterWorldAccounting(ctx)
			positive := mode == "registered" || mode == "second-participant"
			if !positive {
				if err == nil {
					done()
					t.Fatal("unproved writer admitted")
				}
				return
			}
			if err != nil {
				t.Fatal("registered CONFIG blocked the original R0 accounting gate", err)
			}
			done()
			if id.Installation != "installation" || id.Generation != 1 {
				t.Fatal(id)
			}
		})
	}
}
