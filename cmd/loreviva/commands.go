package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func operator(cfg config.Config, s *store.Store) *auth.Operator {
	return auth.New(s, domain.ProductScope{TenantID: cfg.OperatorTenant, ProductID: cfg.OperatorProduct, SubjectID: cfg.OperatorSubject}, cfg.OperatorCredentialID, cfg.OperatorSecret.Reveal())
}
func command(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	s, e := store.Open(cfg.Database)
	if e != nil {
		return errors.New("database configuration unavailable")
	}
	defer s.Close()
	emit := func(v any) error { return json.NewEncoder(out).Encode(v) }
	if len(args) == 0 {
		return errors.New("maintenance command required")
	}
	switch args[0] {
	case "migrate":
		if len(args) < 2 {
			return errors.New("migrate status/plan/up required")
		}
		flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		planID := flags.String("plan-id", "", "pinned migration plan identity")
		if e = flags.Parse(args[2:]); e != nil || flags.NArg() != 0 {
			return errors.New("invalid migration options")
		}
		p, e := maintenance.MigrationPlan(ctx, s)
		if e != nil {
			return errors.New("migration status unavailable or incompatible")
		}
		switch args[1] {
		case "status", "plan":
			return emit(p)
		case "up":
			var hasInstallation bool
			if s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_installation)").Scan(&hasInstallation) == nil && hasInstallation {
				return errors.New("existing installation migration requires closed maintenance adapter")
			}
			if e = maintenance.Up(ctx, s, *planID); e != nil {
				return errors.New("migration not applied; current explicit plan_id required")
			}
			p, e = maintenance.MigrationPlan(ctx, s)
			if e != nil {
				return errors.New("migration verification failed")
			}
			return emit(p)
		default:
			return errors.New("migrate status/plan/up required")
		}
	case "bootstrap":
		flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		mock := flags.Bool("mock", false, "explicit synthetic bootstrap")
		consent := flags.Bool("owner-private-generation-consent", false, "explicit fresh owner private generation opt-in for at most 30 days")
		tenant := flags.String("tenant", "", "synthetic tenant")
		if e = flags.Parse(args[1:]); e != nil || flags.NArg() != 0 || !*mock || *tenant != cfg.OperatorTenant || *tenant != "demo-local" {
			return errors.New("bootstrap requires --mock --tenant demo-local")
		}
		a := operator(cfg, s)
		if e = app.InitializeLocal(ctx, s, cfg.GovernanceRoot, cfg.FilesRoot); e != nil {
			return errors.New("independent fresh bootstrap unavailable")
		}
		if e = maintenance.BootstrapSyntheticWithConsent(ctx, s, a.Scope(), cfg.OperatorCredentialID, cfg.OperatorSecret.Reveal(), *consent); e != nil {
			return errors.New("synthetic bootstrap refused; existing governance and credentials are preserved")
		}
		if e = a.Check(ctx); e != nil {
			return e
		}
		var current bool
		if e = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.generation.use' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))", a.Scope().TenantID, a.Scope().ProductID, a.Scope().SubjectID, a.Scope().SubjectID).Scan(&current); e != nil {
			return e
		}
		return emit(map[string]any{"bootstrap": "fresh-synthetic-owner", "tenant_id": *tenant, "product_id": cfg.OperatorProduct, "credential_reference": cfg.OperatorSecretRef, "mock_only": true, "owner_private_generation_consent_requested": *consent, "owner_private_generation_grant_current": current})
	case "seed":
		local, e := app.OpenLocal(ctx, s, cfg.GovernanceRoot, cfg.FilesRoot)
		if e != nil {
			return e
		}
		defer local.Participant.Close(context.WithoutCancel(ctx))
		if len(args) < 2 || args[1] != "coffee" {
			return errors.New("seed coffee required")
		}
		flags := flag.NewFlagSet("seed", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		mock := flags.Bool("mock", false, "synthetic seed")
		tenant := flags.String("tenant", "", "synthetic tenant")
		if e = flags.Parse(args[2:]); e != nil || flags.NArg() != 0 || !*mock || *tenant != "demo-local" || *tenant != cfg.OperatorTenant {
			return errors.New("seed coffee requires --mock --tenant demo-local")
		}
		a := operator(cfg, s)
		if e = a.Check(ctx); e != nil {
			return errors.New("configured owner authentication required for seed")
		}
		receipt, e := content.New(s).SeedCoffee(ctx, a.Scope(), "coffee-v1")
		if e != nil {
			return errors.New("synthetic seed unavailable")
		}
		return emit(receipt)
	case "maintenance":
		local, e := app.OpenLocal(ctx, s, cfg.GovernanceRoot, cfg.FilesRoot)
		if e != nil {
			return e
		}
		defer local.Participant.Close(context.WithoutCancel(ctx))
		if len(args) < 2 || args[1] != "cleanup-files" {
			return errors.New("maintenance cleanup-files required")
		}
		flags := flag.NewFlagSet("cleanup-files", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		key := flags.String("key", "", "stable command key")
		if e = flags.Parse(args[2:]); e != nil || flags.NArg() != 0 || *key == "" {
			return errors.New("cleanup-files requires --key")
		}
		a := operator(cfg, s)
		if e = a.Check(ctx); e != nil {
			return errors.New("configured owner authentication required")
		}
		receipt, e := app.QueueCleanup(ctx, s, a.Scope(), *key)
		if e != nil {
			return errors.New("cleanup not accepted")
		}
		return emit(receipt)
	default:
		return errors.New("unknown local maintenance command")
	}
}
func runtime(cfg config.Config) (*app.Runtime, func(), error) {
	s, e := store.Open(cfg.Database)
	if e != nil {
		return nil, nil, e
	}
	f, e := files.New(s, cfg.FilesRoot)
	if e != nil {
		s.Close()
		return nil, nil, errors.New("private directory unavailable")
	}
	r := app.NewRuntime(s, f, operator(cfg, s))
	local, e := app.OpenLocal(context.Background(), s, cfg.GovernanceRoot, cfg.FilesRoot)
	if e != nil {
		f.Close()
		s.Close()
		return nil, nil, e
	}
	r.BindLocal(local)
	r.Memory.Ledger = local.Ledger
	r.Worker.Memory = r.Memory
	return r, func() { local.Participant.Close(context.Background()); f.Close(); s.Close() }, nil
}
