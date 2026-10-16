package server

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// This is deliberately neither a bcrypt hash nor a legacy SHA-256 hash. No
// password, including the empty string or this literal, can authenticate it.
const dopOAuthOnlyPassword = "!dop:oauth-only:v1"

// External identities are immutable bindings, not email-based account links.
// Keep tombstones after native user deletion to prevent automatic relinking.
type dopOAuthIdentity struct {
	UserID     string `gorm:"primaryKey"`
	ProviderID string `gorm:"uniqueIndex:dop_oauth_subject;size:128"`
	Subject    string `gorm:"uniqueIndex:dop_oauth_subject;size:512"`
	CreatedAt  time.Time
}

func (dopOAuthIdentity) TableName() string { return "dop_oauth_identities" }

type dopIdentityStore struct {
	*GormStore
	blockManagedUpdates bool
}

func (s *GormStore) dopManagedUser(id string) (bool, error) {
	var count int64
	err := s.db.Model(&dopOAuthIdentity{}).Where("user_id = ?", id).Count(&count).Error
	if err != nil || count != 0 {
		return count != 0, err
	}
	// Fail closed if someone restores native users without extension metadata.
	err = s.db.Model(&AdminUser{}).Where("id = ? AND password_hash = ?", id, dopOAuthOnlyPassword).Count(&count).Error
	return count != 0, err
}

func dopOAuthOnlyError() error {
	return NewHTTPError(403, "oauth_only_account", "This employee account only supports its bound OAuth identity")
}

func (s *dopIdentityStore) AuthenticateAdminUser(identity, password string, ttl time.Duration) (AdminUser, AdminSession, error) {
	var user AdminUser
	identity = strings.ToLower(strings.TrimSpace(identity))
	err := s.db.Select("id", "password_hash").Where("LOWER(email) = ? OR LOWER(username) = ?", identity, identity).Take(&user).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return AdminUser{}, AdminSession{}, dopBatchSafeError(err)
	}
	managed, checkErr := s.dopManagedUser(user.ID)
	if checkErr != nil {
		return AdminUser{}, AdminSession{}, dopBatchSafeError(checkErr)
	}
	if managed || user.PasswordHash == dopOAuthOnlyPassword {
		return AdminUser{}, AdminSession{}, NewHTTPError(401, "invalid_credentials", "Invalid username or password")
	}
	return s.GormStore.AuthenticateAdminUser(identity, password, ttl)
}

func (s *dopIdentityStore) UpdateAdminUser(id string, patch AdminUser, password string) (AdminUser, error) {
	managed, err := s.dopManagedUser(id)
	if err != nil {
		return AdminUser{}, dopBatchSafeError(err)
	}
	if managed && (s.blockManagedUpdates || password != "" || (patch.Role != "" && patch.Role != "user")) {
		return AdminUser{}, dopOAuthOnlyError()
	}
	return s.GormStore.UpdateAdminUser(id, patch, password)
}

func (s *dopIdentityStore) CreateAdminPasswordResetToken(id, createdBy string, ttl time.Duration) (string, AdminPasswordResetToken, error) {
	managed, err := s.dopManagedUser(id)
	if err != nil {
		return "", AdminPasswordResetToken{}, dopBatchSafeError(err)
	}
	if managed {
		return "", AdminPasswordResetToken{}, dopOAuthOnlyError()
	}
	return s.GormStore.CreateAdminPasswordResetToken(id, createdBy, ttl)
}

func (s *dopIdentityStore) ResetAdminUserPassword(token, password string) (AdminUser, error) {
	var reset AdminPasswordResetToken
	err := s.db.Select("user_id").Where("token_hash = ?", HashSecret(token)).Take(&reset).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return AdminUser{}, dopBatchSafeError(err)
	}
	managed, err := s.dopManagedUser(reset.UserID)
	if err != nil {
		return AdminUser{}, dopBatchSafeError(err)
	}
	if managed {
		return AdminUser{}, dopOAuthOnlyError()
	}
	return s.GormStore.ResetAdminUserPassword(token, password)
}
