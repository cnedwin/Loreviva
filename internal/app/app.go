package app

import (
	"context"
	"errors"
	"github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/transport"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/store/mysql"
)

type Dependencies struct {
	API    transport.Server
	Worker transport.Server
	Store  *mysql.Store
	// Readiness must verify the persistence schema, private files and governance watermarks.
	Readiness func(context.Context) error
}

func Run(ctx context.Context, cfg config.Config, deps Dependencies) error {
	var servers []transport.Server
	switch cfg.Mode {
	case "all":
		if deps.API == nil || deps.Worker == nil {
			return errors.New("all mode requires registered API and worker modules")
		}
		servers = []transport.Server{deps.API, deps.Worker}
	case "api":
		if deps.API == nil {
			return errors.New("api mode requires a registered API module")
		}
		servers = []transport.Server{deps.API}
	case "worker":
		if deps.Worker == nil {
			return errors.New("worker mode requires a registered worker module")
		}
		servers = []transport.Server{deps.Worker}
	default:
		return errors.New("mode must be all, api or worker")
	}
	if deps.Readiness == nil {
		return errors.New("persistence readiness check is not registered")
	}
	if deps.Store == nil {
		return errors.New("verified MySQL store is not registered")
	}
	if err := deps.Store.Readiness(ctx, deps.Readiness); err != nil {
		return errors.New("startup prerequisites failed; application remains unready")
	}
	return kratos.New(kratos.Name("loreviva"), kratos.Context(ctx), kratos.Server(servers...)).Run()
}
