package app

import (
	"context"
	"errors"
	"loreviva.local/loreviva/internal/config"
	"testing"
)

func TestRunRejectsUnregisteredModules(t *testing.T) {
	for _, mode := range []string{"all", "api", "worker", "bogus"} {
		if Run(context.Background(), config.Config{Mode: mode}, Dependencies{}) == nil {
			t.Fatalf("unregistered %s accepted", mode)
		}
	}
}

type probeServer struct{ started bool }

func (s *probeServer) Start(context.Context) error {
	s.started = true
	return errors.New("probe stopped")
}
func (s *probeServer) Stop(context.Context) error { return nil }
func TestRunRejectsMissingMySQLBeforeTransport(t *testing.T) {
	s := &probeServer{}
	e := Run(context.Background(), config.Config{Mode: "api"}, Dependencies{API: s, Readiness: func(context.Context) error { return nil }})
	if e == nil || s.started {
		t.Fatal("transport started before verified MySQL store")
	}
}
