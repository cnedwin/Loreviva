package app

import (
	"context"
	"crypto/cipher"
	"errors"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/delivery"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net"
	"net/http"
	"sync"
)

type Runtime struct {
	Delivery          *delivery.Service
	deliveryBindError error
	creationStore     *store.Store
	adminCursorOnce   sync.Once
	adminCursor       cipher.AEAD
	adminCursorErr    error
	Candidates        *harness.CandidateService
	Turns             *harness.TurnService
	Local             *Local
	Memory            *memory.MemoryService
	World             *world.Runtime
	Store             *store.Store
	Files             *files.Store
	Auth              *auth.Operator
	Content           *content.Service
	Worker            *Worker
}

func NewRuntime(s *store.Store, f *files.Store, a *auth.Operator) *Runtime {
	s.RehearsalPurgeOwner = f
	s.PackageApprovalFiles = f
	s.DraftRehearsalOwner = content.New(s)
	s.DraftRehearsalReader = f
	s.RehearsalReportReader = f
	s.DraftCandidateOwner = content.New(s)
	s.DraftCandidateReader = f
	m := memory.New(s, nil)
	s.StudioConsentOwner = m
	m.Files = f
	s.GenerationContext = m
	s.TurnOwner = m
	f.MemoryPolicy = m
	f.OriginalResultPolicy = m
	s.OriginalResultReader = f
	s.ApprovedTurnReader = f
	s.OriginalResultProgram = harness.OriginalResultProgram{}
	s.CandidateOwner = &harness.CandidateOwner{Store: s}
	w := world.New(s)
	s.TurnSkills = w
	w.InitialMemory = m
	w.RehearsalMemory = m
	s.RehearsalInputOwner = m
	s.RehearsalInitializer = w
	worker := NewWorker(s, f)
	worker.Memory = m
	author := content.New(s)
	author.PackageFiles = f
	return &Runtime{creationStore: s, Turns: harness.NewTurnService(s), Store: s, Files: f, Auth: a, Content: author, World: w, Memory: m, Worker: worker}
}

func (r *Runtime) BindLocal(local *Local) {
	r.Local = local
	if local == nil {
		r.Candidates = nil
		r.Worker.Turns = nil
		return
	}
	r.Memory.Ledger = local.Ledger
	// The executable opens Local after NewRuntime. Bind the creation owners
	// only here, after real admission exists; repeated calls cannot replace them.
	if local.creationStore == r.creationStore && r.Store == r.creationStore && r.World.Store == r.creationStore && r.Memory.Store == r.creationStore {
		r.deliveryBindError = r.Store.BindDeliveryAdmission(local.Participant)
		_ = r.World.BindIntegrationCreation(r.Memory)
		_ = r.Store.BindIntegrationConsentOwner(context.Background(), r.Memory)
		if r.deliveryBindError == nil {
			r.deliveryBindError = r.World.BindIntegrationDelivery(context.Background())
		}
		if r.deliveryBindError == nil && r.Turns.Store == r.Store && r.Files.DB == r.Store {
			r.Delivery, r.deliveryBindError = delivery.New(r.Store, r.Turns, local.Participant, r.World)
		}
	}
	r.Worker.Turns = &harness.Executor{Turns: r.Turns, Files: r.Files, Journal: local.Mock, Participant: local.Participant}
	r.Candidates = &harness.CandidateService{Executor: r.Worker.Turns}
}
func (r *Runtime) Readiness(ctx context.Context) error { return r.readiness(ctx, false) }

// Readiness verifies the registered prerequisites; the original-governance path skips only the selected Mock continuity/result-governance checks, not request authorization.
func (r *Runtime) readiness(ctx context.Context, originalGovernance bool) error {
	if r == nil || r.Store == nil || r.Files == nil || r.Auth == nil || r.Worker == nil {
		return errors.New("M1 dependencies unavailable")
	}
	if r.deliveryBindError != nil {
		return r.deliveryBindError
	}
	if r.Local != nil {
		if e := r.Local.Participant.Check(ctx); e != nil {
			return e
		}
		if !originalGovernance {
			if r.Local.Mock == nil {
				return errors.New("original Mock continuity unavailable")
			}
			if e := r.Local.Mock.Ready(ctx); e != nil {
				return e
			}
			if e := r.Memory.CheckResultGovernance(ctx); e != nil {
				return e
			}
		}
	}
	for _, check := range []func(context.Context) error{func(ctx context.Context) error { return maintenance.Compatible(ctx, r.Store) }, r.Files.Check, func(ctx context.Context) error { return maintenance.GovernanceReady(ctx, r.Store) }, r.Auth.RegistryReady, r.Worker.Check} {
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
