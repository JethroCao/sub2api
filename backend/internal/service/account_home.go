package service

import (
	"context"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
	"reflect"
	"strings"
)

const OpenAICredentialSourceKey = "openai_credential_source"
const HomeCredentialIDKey = "home_credential_id"

var ErrHomeBinding = infraerrors.BadRequest("HOME_BINDING_INVALID", "Home binding requires an ordinary OpenAI OAuth owner and a valid UUID")
var ErrHomeOwnershipChanged = infraerrors.Conflict("HOME_OWNERSHIP_CHANGED", "credential ownership changed; reload the account")

func HasHomeBindingKeys(extra map[string]any) bool {
	_, a := extra[OpenAICredentialSourceKey]
	_, b := extra[HomeCredentialIDKey]
	return a || b
}

// HasHomeBinding deliberately recognizes malformed metadata regardless of type/provider.
func (a *Account) HasHomeBinding() bool { return a != nil && HasHomeBindingKeys(a.Extra) }
func (a *Account) HomeCredentialID() (string, error) {
	if !a.HasHomeBinding() {
		return "", nil
	}
	source, ok := a.Extra[OpenAICredentialSourceKey].(string)
	id, idok := a.Extra[HomeCredentialIDKey].(string)
	parsed, err := uuid.Parse(id)
	if !ok || source != "home" || !idok || err != nil || parsed == uuid.Nil || id != parsed.String() || a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth || a.IsCredentialShadow() {
		return "", ErrHomeBinding
	}
	for _, key := range []string{"auth_mode", "openai_auth_mode"} {
		mode := strings.ToLower(strings.TrimSpace(a.GetCredential(key)))
		if mode != "" && mode != "oauth" {
			return "", ErrHomeBinding
		}
	}
	for key, value := range a.Credentials {
		if (strings.HasPrefix(key, "agent_") || key == "task_id") && value != nil && value != "" {
			return "", ErrHomeBinding
		}
	}
	return id, nil
}

// HomeAuthCredentialKeys is shared with atomic repository sanitization.
var HomeAuthCredentialKeys = []string{"access_token", "refresh_token", "id_token", "api_key", "auth_mode", "openai_auth_mode", "agent_private_key", "agent_runtime", "agent_task", "agent_task_id", "agent_runtime_id", "agent_id", "task_id", "client_id", "chatgpt_account_id", "chatgpt_user_id", "organization_id", "expires_at"}

func SanitizeHomeCredentials(creds map[string]any) map[string]any {
	out := shallowCopyMap(creds)
	for _, k := range HomeAuthCredentialKeys {
		delete(out, k)
	}
	for k := range out {
		if strings.HasPrefix(k, "agent_") {
			delete(out, k)
		}
	}
	return out
}
func ValidateAndSanitizeHomeAccount(a *Account) error {
	id, err := a.HomeCredentialID()
	if err != nil {
		return err
	}
	if id != "" {
		a.Credentials = SanitizeHomeCredentials(a.Credentials)
	}
	return nil
}
func RejectHomeBindingMutation(extra map[string]any) error {
	if HasHomeBindingKeys(extra) {
		return ErrHomeBinding
	}
	return nil
}

type homeBindingMutationKey struct{}
type homeBindingMutation struct {
	id     int64
	before map[string]any
}

func homeBindingValues(extra map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{OpenAICredentialSourceKey, HomeCredentialIDKey} {
		if v, ok := extra[k]; ok {
			out[k] = v
		}
	}
	return out
}
func prepareHomeBindingUpdate(ctx context.Context, a *Account, input map[string]any) (context.Context, error) {
	if !HasHomeBindingKeys(input) {
		return ctx, nil
	}
	source, s := input[OpenAICredentialSourceKey]
	id, i := input[HomeCredentialIDKey]
	if !s || !i {
		return ctx, ErrHomeBinding
	}
	if (source == nil || source == "native") && (id == nil || id == "") {
		delete(input, OpenAICredentialSourceKey)
		delete(input, HomeCredentialIDKey)
	}
	return context.WithValue(ctx, homeBindingMutationKey{}, homeBindingMutation{a.ID, homeBindingValues(a.Extra)}), nil
}

// ProtectHomeAccountWrite runs under the repository row lock. Only the validated
// single-account edit can alter the pair; stale full writes cannot erase ownership.
func ProtectHomeAccountWrite(ctx context.Context, current map[string]any, a *Account) error {
	if required, _ := ctx.Value(nativeCredentialMutationKey{}).(bool); required && HasHomeBindingKeys(current) {
		return ErrHomeOwnershipChanged
	}
	old := homeBindingValues(current)
	next := homeBindingValues(a.Extra)
	intent, explicit := ctx.Value(homeBindingMutationKey{}).(homeBindingMutation)
	if explicit && intent.id == a.ID {
		if !reflect.DeepEqual(intent.before, old) {
			return ErrHomeOwnershipChanged
		}
	} else if !reflect.DeepEqual(old, next) {
		return ErrHomeOwnershipChanged
	}
	return ValidateAndSanitizeHomeAccount(a)
}

type nativeCredentialMutationKey struct{}

type homeResultCredentialKey struct{}
type homeResultBinding struct {
	credentialID string
	selectedID   int64
	ownerID      int64
}

// WithHomeResultCredentialID pins durable response-state mutations to the Home
// binding that produced the response. Repositories enforce it at the write.
func WithHomeResultCredentialID(ctx context.Context, id string) context.Context {
	return WithHomeResultBinding(ctx, id, 0, 0)
}

func WithHomeResultBinding(ctx context.Context, id string, selectedID, ownerID int64) context.Context {
	return context.WithValue(ctx, homeResultCredentialKey{}, homeResultBinding{id, selectedID, ownerID})
}

func HomeResultAccountID(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	binding, ok := ctx.Value(homeResultCredentialKey{}).(homeResultBinding)
	return binding.selectedID, ok && binding.selectedID > 0
}

func HomeResultOwnerID(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	binding, ok := ctx.Value(homeResultCredentialKey{}).(homeResultBinding)
	return binding.ownerID, ok && binding.ownerID > 0
}

func HomeResultCredentialID(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	binding, ok := ctx.Value(homeResultCredentialKey{}).(homeResultBinding)
	return binding.credentialID, ok
}

// WithNativeCredentialMutation prevents re-auth from racing an ownership conversion.
func WithNativeCredentialMutation(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeCredentialMutationKey{}, true)
}
func requireNativeCredentialMutation(ctx context.Context, a *Account) error {
	if required, _ := ctx.Value(nativeCredentialMutationKey{}).(bool); required && a.HasHomeBinding() {
		return ErrHomeOwnershipChanged
	}
	return nil
}
