package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/qiffang/mnemos/server/internal/domain"
	"github.com/qiffang/mnemos/server/internal/repository"
	"github.com/qiffang/mnemos/server/internal/tenant"
)

const (
	tenantMemorySchema = `CREATE TABLE IF NOT EXISTS memories (
	    id              VARCHAR(36)     PRIMARY KEY,
	    content         TEXT            NOT NULL,
	    source          VARCHAR(100),
	    tags            JSON,
	    metadata        JSON,
	    embedding       VECTOR(1536)    NULL,
	    memory_type     VARCHAR(20)     NOT NULL DEFAULT 'pinned',
	    agent_id        VARCHAR(100)    NULL,
	    session_id      VARCHAR(100)    NULL,
	    state           VARCHAR(20)     NOT NULL DEFAULT 'active',
	    version         INT             DEFAULT 1,
	    updated_by      VARCHAR(100),
	    superseded_by   VARCHAR(36)     NULL,
	    created_at      TIMESTAMP       DEFAULT CURRENT_TIMESTAMP,
	    updated_at      TIMESTAMP       DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	    INDEX idx_memory_type         (memory_type),
	    INDEX idx_source              (source),
	    INDEX idx_state               (state),
	    INDEX idx_agent               (agent_id),
	    INDEX idx_session             (session_id),
	    INDEX idx_updated             (updated_at)
	)`
)

type TenantService struct {
	tenants repository.TenantRepo
	tokens  repository.TenantTokenRepo
	zero    *tenant.ZeroClient
	pool    *tenant.TenantPool
	logger  *slog.Logger
}

func NewTenantService(
	tenants repository.TenantRepo,
	tokens repository.TenantTokenRepo,
	zero *tenant.ZeroClient,
	pool *tenant.TenantPool,
	logger *slog.Logger,
) *TenantService {
	return &TenantService{tenants: tenants, tokens: tokens, zero: zero, pool: pool, logger: logger}
}

type RegisterInput struct {
	Name string `json:"name"`
}

type RegisterResult struct {
	TenantID string              `json:"tenant_id"`
	Token    string              `json:"token"`
	ClaimURL string              `json:"claim_url,omitempty"`
	Status   domain.TenantStatus `json:"status"`
}

func (s *TenantService) Register(ctx context.Context, input RegisterInput) (*RegisterResult, error) {
	if err := validateTenantInput(input.Name); err != nil {
		return nil, err
	}

	existing, err := s.tenants.GetByName(ctx, input.Name)
	if err == nil {
		if existing.Status != domain.TenantDeleted {
			token, err := s.createToken(ctx, existing.ID)
			if err != nil {
				return nil, err
			}
			return &RegisterResult{
				TenantID: existing.ID,
				Token:    token,
				ClaimURL: existing.ClaimURL,
				Status:   existing.Status,
			}, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	if s.zero == nil {
		return nil, &domain.ValidationError{Message: "provisioning disabled"}
	}

	tenantID := "t_" + uuid.New().String()
	instance, err := s.zero.CreateInstance(ctx, "mnemos-"+tenantID)
	if err != nil {
		t := &domain.Tenant{
			ID:            tenantID,
			Name:          input.Name,
			DBHost:        "",
			DBPort:        0,
			DBUser:        "",
			DBPassword:    "",
			DBName:        "",
			DBTLS:         true,
			Provider:      "tidb_zero",
			Status:        domain.TenantProvisioning,
			SchemaVersion: 0,
		}
		if createErr := s.tenants.Create(ctx, t); createErr != nil {
			return nil, createErr
		}
		return &RegisterResult{TenantID: tenantID, Status: domain.TenantProvisioning}, err
	}

	t := &domain.Tenant{
		ID:            tenantID,
		Name:          input.Name,
		DBHost:        instance.Host,
		DBPort:        instance.Port,
		DBUser:        instance.Username,
		DBPassword:    instance.Password,
		DBName:        "test",
		DBTLS:         true,
		Provider:      "tidb_zero",
		ClusterID:     instance.ID,
		ClaimURL:      instance.ClaimURL,
		Status:        domain.TenantProvisioning,
		SchemaVersion: 0,
	}
	if err := s.tenants.Create(ctx, t); err != nil {
		return nil, err
	}

	if err := s.initSchema(ctx, t); err != nil {
		if s.logger != nil {
			s.logger.Error("tenant schema init failed", "tenant_id", tenantID, "err", err)
		}
		return nil, err
	}

	if err := s.tenants.UpdateStatus(ctx, tenantID, domain.TenantActive); err != nil {
		return nil, err
	}
	if err := s.tenants.UpdateSchemaVersion(ctx, tenantID, 1); err != nil {
		return nil, err
	}

	token, err := s.createToken(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	return &RegisterResult{
		TenantID: tenantID,
		Token:    token,
		ClaimURL: instance.ClaimURL,
		Status:   domain.TenantActive,
	}, nil
}

func (s *TenantService) AddToken(ctx context.Context, tenantID string) (string, error) {
	t, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if t.Status != domain.TenantActive {
		return "", &domain.ValidationError{Message: "tenant not active"}
	}

	return s.createToken(ctx, tenantID)
}

func (s *TenantService) GetInfo(ctx context.Context, tenantID string) (*domain.TenantInfo, error) {
	t, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	tokens, err := s.tokens.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	if s.pool == nil {
		return nil, fmt.Errorf("tenant pool not configured")
	}
	db, err := s.pool.Get(ctx, tenantID, t.DSN())
	if err != nil {
		return nil, err
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories").Scan(&count); err != nil {
		return nil, err
	}

	return &domain.TenantInfo{
		TenantID:    t.ID,
		Name:        t.Name,
		Status:      t.Status,
		Provider:    t.Provider,
		ClaimURL:    t.ClaimURL,
		AgentCount:  len(tokens),
		MemoryCount: count,
		CreatedAt:   t.CreatedAt,
	}, nil
}

func (s *TenantService) initSchema(ctx context.Context, t *domain.Tenant) error {
	if s.pool == nil {
		return fmt.Errorf("tenant pool not configured")
	}
	db, err := s.pool.Get(ctx, t.ID, t.DSN())
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, tenantMemorySchema); err != nil {
		return fmt.Errorf("init tenant schema: memories: %w", err)
	}
	return nil
}

func (s *TenantService) createToken(ctx context.Context, tenantID string) (string, error) {
	token, err := domain.GenerateToken()
	if err != nil {
		return "", err
	}
	tt := &domain.TenantToken{
		APIToken: token,
		TenantID: tenantID,
	}
	if err := s.tokens.CreateToken(ctx, tt); err != nil {
		return "", err
	}
	return token, nil
}

func validateTenantInput(name string) error {
	if name == "" {
		return &domain.ValidationError{Field: "name", Message: "required"}
	}
	if len(name) > 255 {
		return &domain.ValidationError{Field: "name", Message: "too long (max 255)"}
	}
	return nil
}
