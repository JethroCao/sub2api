package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type authEmailPasswordSettingRepo struct {
	values map[string]string
}

func (r *authEmailPasswordSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	panic("unexpected Get call")
}

func (r *authEmailPasswordSettingRepo) GetValue(context.Context, string) (string, error) {
	panic("unexpected GetValue call")
}

func (r *authEmailPasswordSettingRepo) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}

func (r *authEmailPasswordSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *authEmailPasswordSettingRepo) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (r *authEmailPasswordSettingRepo) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (r *authEmailPasswordSettingRepo) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

func newAuthHandlerWithEmailPasswordSettings(values map[string]string) *AuthHandler {
	return &AuthHandler{
		settingSvc: service.NewSettingService(&authEmailPasswordSettingRepo{values: values}, &config.Config{}),
	}
}

func TestEnsureEmailPasswordLoginAllowsUser_DefaultAllowsRegularUser(t *testing.T) {
	h := newAuthHandlerWithEmailPasswordSettings(map[string]string{})

	err := h.ensureEmailPasswordLoginAllowsUser(context.Background(), &service.User{ID: 1, Role: service.RoleUser})

	require.NoError(t, err)
}

func TestEnsureEmailPasswordLoginAllowsUser_AdminFallback(t *testing.T) {
	h := newAuthHandlerWithEmailPasswordSettings(map[string]string{
		service.SettingKeyEmailPasswordLoginEnabled:      "false",
		service.SettingKeyAdminEmailLoginFallbackEnabled: "true",
	})

	err := h.ensureEmailPasswordLoginAllowsUser(context.Background(), &service.User{ID: 1, Role: service.RoleAdmin})

	require.NoError(t, err)
}

func TestEnsureEmailPasswordLoginAllowsUser_RejectsRegularUserWhenDisabled(t *testing.T) {
	h := newAuthHandlerWithEmailPasswordSettings(map[string]string{
		service.SettingKeyEmailPasswordLoginEnabled:      "false",
		service.SettingKeyAdminEmailLoginFallbackEnabled: "true",
	})

	err := h.ensureEmailPasswordLoginAllowsUser(context.Background(), &service.User{ID: 1, Role: service.RoleUser})

	require.Error(t, err)
	require.Equal(t, "EMAIL_PASSWORD_LOGIN_DISABLED", infraerrors.Reason(err))
}

func TestEnsureEmailPasswordLoginAllowsUser_RejectsAdminWhenFallbackDisabled(t *testing.T) {
	h := newAuthHandlerWithEmailPasswordSettings(map[string]string{
		service.SettingKeyEmailPasswordLoginEnabled:      "false",
		service.SettingKeyAdminEmailLoginFallbackEnabled: "false",
	})

	err := h.ensureEmailPasswordLoginAllowsUser(context.Background(), &service.User{ID: 1, Role: service.RoleAdmin})

	require.Error(t, err)
	require.Equal(t, "EMAIL_PASSWORD_LOGIN_DISABLED", infraerrors.Reason(err))
}
