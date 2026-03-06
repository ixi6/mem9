package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/qiffang/mnemos/server/internal/domain"
	"github.com/qiffang/mnemos/server/internal/repository"
	"github.com/qiffang/mnemos/server/internal/tenant"
)

type contextKey string

const authInfoKey contextKey = "authInfo"

const AgentIDHeader = "X-Mnemo-Agent-Id"

func Auth(
	tenantTokens repository.TenantTokenRepo,
	tenantRepo repository.TenantRepo,
	pool *tenant.TenantPool,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearerToken(r)
			if token == "" {
				writeError(w, http.StatusUnauthorized, "missing authorization token")
				return
			}

			info, err := resolveToken(r.Context(), token, tenantTokens, tenantRepo, pool)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid token")
				return
			}

			if agentID := r.Header.Get(AgentIDHeader); agentID != "" {
				info.AgentName = agentID
			}

			ctx := context.WithValue(r.Context(), authInfoKey, info)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func resolveToken(
	ctx context.Context,
	token string,
	tenantTokens repository.TenantTokenRepo,
	tenantRepo repository.TenantRepo,
	pool *tenant.TenantPool,
) (*domain.AuthInfo, error) {
	tt, err := tenantTokens.GetByToken(ctx, token)
	if err != nil {
		return nil, err
	}

	t, tErr := tenantRepo.GetByID(ctx, tt.TenantID)
	if tErr != nil {
		return nil, tErr
	}
	if t.Status != domain.TenantActive {
		return nil, errors.New("tenant not active")
	}
	db, dbErr := pool.Get(ctx, t.ID, t.DSN())
	if dbErr != nil {
		return nil, dbErr
	}
	return &domain.AuthInfo{
		TenantID: tt.TenantID,
		TenantDB: db,
	}, nil
}

func AuthFromContext(ctx context.Context) *domain.AuthInfo {
	info, _ := ctx.Value(authInfoKey).(*domain.AuthInfo)
	return info
}

func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
