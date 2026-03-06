package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/qiffang/mnemos/server/internal/domain"
)

func TestValidateTenantInput_Valid(t *testing.T) {
	if err := validateTenantInput("tenant"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTenantInput_EmptyName(t *testing.T) {
	err := validateTenantInput("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var vErr *domain.ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation unwrap")
	}
	if vErr.Field != "name" {
		t.Fatalf("field = %q, want %q", vErr.Field, "name")
	}
	if vErr.Message != "required" {
		t.Fatalf("message = %q, want %q", vErr.Message, "required")
	}
}

func TestValidateTenantInput_NameTooLong(t *testing.T) {
	name := strings.Repeat("a", 256)
	err := validateTenantInput(name)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var vErr *domain.ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if vErr.Field != "name" {
		t.Fatalf("field = %q, want %q", vErr.Field, "name")
	}
	if vErr.Message != "too long (max 255)" {
		t.Fatalf("message = %q, want %q", vErr.Message, "too long (max 255)")
	}
}

func TestTenantSchemaConstants(t *testing.T) {
	memoryChecks := []string{
		"CREATE TABLE IF NOT EXISTS memories",
		"id              VARCHAR(36)",
		"embedding       VECTOR(1536)",
		"INDEX idx_updated",
	}
	for _, needle := range memoryChecks {
		if !strings.Contains(tenantMemorySchema, needle) {
			t.Fatalf("tenantMemorySchema missing %q", needle)
		}
	}
}
