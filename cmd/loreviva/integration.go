package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"time"
)

// Only this explicit local command exposes the closed administrative registry.
// Reading a request or a key never supplies authority: the owning transaction
// rechecks the authenticated registrar and its current CONFIG admission.
func integrationCommand(ctx context.Context, cfg config.Config, s *store.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("integration init-registrar/registry/receipt required")
	}
	flags := flag.NewFlagSet("integration", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	key := flags.String("key", "", "original command key")
	var installation, request, secret *string
	switch args[0] {
	case "init-registrar":
		installation = flags.String("installation", "", "current installation identity")
	case "registry":
		request = flags.String("request-file", "", "private closed command JSON")
		secret = flags.String("secret-file", "", "private service key file")
	case "receipt":
	default:
		return errors.New("integration init-registrar/registry/receipt required")
	}
	if e := flags.Parse(args[1:]); e != nil || flags.NArg() != 0 || domain.ValidateIdempotencyKey(*key) != nil {
		return errors.New("invalid integration command options")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	local, e := app.OpenLocal(ctx, s, cfg.GovernanceRoot, cfg.FilesRoot)
	if e != nil {
		return e
	}
	defer local.Participant.Close(context.WithoutCancel(ctx))
	ctx, _, e = operator(cfg, s).Authenticate(ctx, "Bearer "+cfg.OperatorSecret.Reveal())
	if e != nil {
		return errors.New("current local operator authentication required")
	}
	ctx, _, release, e := s.EnterConfiguration(ctx)
	if e != nil {
		return e
	}
	defer release()
	var receipt domain.RegistryReceipt
	switch args[0] {
	case "init-registrar":
		receipt, e = s.InitializeIntegrationRegistrar(ctx, *key, *installation)
	case "receipt":
		receipt, e = s.ReadIntegrationRegistryReceipt(ctx, *key)
	case "registry":
		var c domain.RegistryCommand
		c, e = readIntegrationCommand(*request)
		if e != nil {
			return errors.New("closed local registry request required")
		}
		if c.Customer != nil {
			c.Customer.SecretHandleRef = *secret
		} else if c.Rotation != nil {
			c.Rotation.SecretHandleRef = *secret
		} else if *secret != "" {
			return errors.New("secret file is not valid for this command")
		}
		receipt, e = s.ApplyIntegrationRegistry(ctx, *key, c)
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(out).Encode(receipt)
}

func readIntegrationCommand(path string) (domain.RegistryCommand, error) {
	var c domain.RegistryCommand
	if !filepath.IsAbs(path) {
		return c, store.ErrDenied
	}
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() < 2 || info.Size() > 16384 {
		return c, store.ErrDenied
	}
	f, e := os.Open(path)
	if e != nil {
		return c, store.ErrDenied
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(info, opened) {
		return c, store.ErrDenied
	}
	decoder := json.NewDecoder(io.LimitReader(f, 16385))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&c); e != nil {
		return c, store.ErrDenied
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return c, store.ErrDenied
	}
	return c, nil
}
