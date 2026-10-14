// Package settings exposes only the seven closed generated Studio operations.
package settings

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

type Service struct{ Store *store.Store }

func New(s *store.Store) *Service { return &Service{Store: s} }
func (s *Service) Read(ctx context.Context, scope domain.SettingsScope) (generated.StudioSettingsSnapshot, error) {
	return s.Store.ReadSettings(ctx, scope)
}
func (s *Service) Command(ctx context.Context, key string, r generated.StudioSettingsCommandRequest) (generated.StudioLocalCommandReceipt, error) {
	return s.Store.ApplySettings(ctx, key, r)
}
func (s *Service) Members(ctx context.Context, scope domain.SettingsScope, limit int, cursor string) (generated.StudioMemberPage, error) {
	return s.Store.ListMembers(ctx, scope, limit, cursor)
}
func (s *Service) MemberCommand(ctx context.Context, key string, r generated.StudioMemberCommandRequest) (generated.StudioLocalCommandReceipt, error) {
	return s.Store.ApplyMember(ctx, key, r)
}
func (s *Service) SelfConsent(ctx context.Context, scope domain.SettingsScope) (generated.StudioSelfConsentSnapshot, error) {
	return s.Store.ReadSelfConsent(ctx, scope)
}
func (s *Service) ConsentCommand(ctx context.Context, key string, r generated.StudioSelfConsentCommandRequest) (generated.StudioLocalCommandReceipt, error) {
	return s.Store.ApplySelfConsent(ctx, key, r)
}
func (s *Service) Receipt(ctx context.Context, scope domain.SettingsScope, family, key string) (generated.StudioLocalCommandReceipt, error) {
	return s.Store.ReadSettingsCommand(ctx, scope, family, key)
}
