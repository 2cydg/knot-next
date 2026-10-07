package secret

import (
	"knot-core/pkg/config"
	"knot-core/pkg/crypto"
)

type Service struct {
	config *config.Service
	crypto crypto.Provider
}

func NewService(configService *config.Service, provider crypto.Provider) *Service {
	return &Service{config: configService, crypto: provider}
}

func (s *Service) CryptoCapability() crypto.Capability {
	return crypto.CapabilityOf(s.crypto)
}

func (s *Service) Summary() (map[string]any, error) {
	return s.config.SecretSummary()
}

func (s *Service) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	return s.config.SetServerPassword(id, password)
}

func (s *Service) ClearServerPassword(id string) (config.ServerProfileView, error) {
	return s.config.ClearServerPassword(id)
}

func (s *Service) SetKeyPrivate(id string, privateKey string, sourcePath string) (config.KeyMetadataView, error) {
	return s.config.SetKeyPrivate(id, privateKey, sourcePath)
}

func (s *Service) ClearKeyPrivate(id string) (config.KeyMetadataView, error) {
	return s.config.ClearKeyPrivate(id)
}

func (s *Service) SetProxyPassword(id string, password string) (config.ProxyProfileView, error) {
	return s.config.SetProxyPassword(id, password)
}

func (s *Service) ClearProxyPassword(id string) (config.ProxyProfileView, error) {
	return s.config.ClearProxyPassword(id)
}

func (s *Service) SetSyncPassword(password string) (config.SettingsView, error) {
	return s.config.SetSyncPassword(password)
}

func (s *Service) ClearSyncPassword() (config.SettingsView, error) {
	return s.config.ClearSyncPassword()
}

func (s *Service) SetSyncProviderPassword(id string, password string) (config.SyncProviderView, error) {
	return s.config.SetSyncProviderPassword(id, password)
}

func (s *Service) ClearSyncProviderPassword(id string) (config.SyncProviderView, error) {
	return s.config.ClearSyncProviderPassword(id)
}

func (s *Service) SetSyncProviderS3Credentials(id string, accessKeyID string, secretAccessKey string, sessionToken string) (config.SyncProviderView, error) {
	return s.config.SetSyncProviderS3Credentials(id, accessKeyID, secretAccessKey, sessionToken)
}

func (s *Service) ClearSyncProviderS3Credentials(id string) (config.SyncProviderView, error) {
	return s.config.ClearSyncProviderS3Credentials(id)
}

func (s *Service) SetKeyPrivateWithPassphrase(id, privateKey, sourcePath, passphrase string) (config.KeyMetadataView, error) {
	return s.config.SetKeyPrivateWithPassphrase(id, privateKey, sourcePath, passphrase)
}
