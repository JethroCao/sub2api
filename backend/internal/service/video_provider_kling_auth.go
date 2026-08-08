package service

import (
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	klingJWTLifetime      = 30 * time.Minute
	klingJWTRefreshBefore = 5 * time.Minute
)

// KlingClock is injected so JWT claims and cache refreshes are deterministic.
type KlingClock interface {
	Now() time.Time
}

type klingSystemClock struct{}

func (klingSystemClock) Now() time.Time { return time.Now() }

// SignKlingJWT creates the legacy Kling Open Platform AK/SK token documented
// at https://kling.ai/document-api/api/get-started/authentication.
func SignKlingJWT(accessKey, secretKey string, clock KlingClock) (string, error) {
	accessKey = strings.TrimSpace(accessKey)
	secretKey = strings.TrimSpace(secretKey)
	if accessKey == "" || secretKey == "" {
		return "", errors.New("kling credentials are required")
	}
	if clock == nil {
		clock = klingSystemClock{}
	}
	now := clock.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    accessKey,
		ExpiresAt: jwt.NewNumericDate(now.Add(klingJWTLifetime)),
		NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secretKey))
	if err != nil {
		return "", errors.New("failed to sign kling authorization token")
	}
	return signed, nil
}
