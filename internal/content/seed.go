package content

import (
	"context"
	"loreviva.local/loreviva/examples/coffee"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

type SeedReceipt struct {
	TenantID    string `json:"tenant_id"`
	ProductID   string `json:"product_id"`
	SeedVersion string `json:"seed_version"`
	DraftID     string `json:"draft_id"`
	ReleaseID   string `json:"release_id"`
	MockOnly    bool   `json:"mock_only"`
}

func (s *Service) SeedCoffee(ctx context.Context, scope domain.ProductScope, version string) (SeedReceipt, error) {
	out := SeedReceipt{}
	if scope.TenantID != "demo-local" || scope.ProductID != "coffee" || version != "coffee-v1" {
		return out, ErrBlocked
	}
	var in generated.DraftInput
	if e := generated.Decode("DraftInput", coffee.Definitions, &in); e != nil {
		return out, e
	}
	prefix := "seed." + version
	d, e := s.Create(ctx, scope, "WORLD", "", in, prefix+".draft")
	if e != nil {
		return out, e
	}
	check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, prefix+".check")
	if e != nil {
		return out, e
	}
	release, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, prefix+".release")
	if e != nil {
		return out, e
	}
	return SeedReceipt{scope.TenantID, scope.ProductID, version, string(d.DraftID), string(release.ReleaseID), true}, nil
}
