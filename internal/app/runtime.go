package app

import (
	"context"
	"errors"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net"
	"net/http"
)

type Runtime struct {
	Local   *Local
	Memory  *memory.MemoryService
	World   *world.Runtime
	Store   *store.Store
	Files   *files.Store
	Auth    *auth.Operator
	Content *content.Service
	Worker  *Worker
}

func NewRuntime(s *store.Store, f *files.Store, a *auth.Operator) *Runtime {
	m := memory.New(s, nil)
	m.Files = f
	f.MemoryPolicy = m
	w := world.New(s)
	w.InitialMemory = m
	worker := NewWorker(s, f)
	worker.Memory = m
	return &Runtime{Store: s, Files: f, Auth: a, Content: content.New(s), World: w, Memory: m, Worker: worker}
}
func (r *Runtime) Readiness(ctx context.Context) error { return r.readiness(ctx, false) }
func (r *Runtime) readiness(ctx context.Context, originalGovernance bool) error {
	if r == nil || r.Store == nil || r.Files == nil || r.Auth == nil || r.Worker == nil {
		return errors.New("M1 dependencies unavailable")
	}
	if r.Local != nil {
		if e := r.Local.Participant.Check(ctx); e != nil {
			return e
		}
		if !originalGovernance {
			if e := r.Local.Ledger.Ready(ctx); e != nil {
				return e
			}
		}
	}
	for _, check := range []func(context.Context) error{func(ctx context.Context) error { return maintenance.Compatible(ctx, r.Store) }, r.Files.Check, func(ctx context.Context) error { return maintenance.GovernanceReady(ctx, r.Store) }, r.Auth.Check, r.Worker.Check} {
		if e := check(ctx); e != nil {
			return errors.New("M1 prerequisites unavailable; no business accepted")
		}
	}
	return nil
}
func (r *Runtime) Dependencies(cfg config.Config) Dependencies {
	return Dependencies{API: &HTTPServer{server: &http.Server{Addr: cfg.ListenAddr, Handler: r.Handler(), ReadHeaderTimeout: 5e9, ReadTimeout: 30e9, WriteTimeout: 30e9}}, Worker: r.Worker, Store: r.Store, Readiness: r.Readiness}
}

type HTTPServer struct{ server *http.Server }

func (s *HTTPServer) Start(ctx context.Context) error {
	listener, e := net.Listen("tcp", s.server.Addr)
	if e != nil {
		return errors.New("HTTP listener unavailable")
	}
	go func() { _ = s.server.Serve(listener) }()
	return nil
}
func (s *HTTPServer) Stop(ctx context.Context) error { return s.server.Shutdown(ctx) }
