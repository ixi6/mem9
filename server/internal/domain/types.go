package domain

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// MemoryType classifies how a memory was created.
type MemoryType string

const (
	TypePinned  MemoryType = "pinned"
	TypeInsight MemoryType = "insight"
)

// MemoryState represents the lifecycle state of a memory.
type MemoryState string

const (
	StateActive   MemoryState = "active"
	StatePaused   MemoryState = "paused"
	StateArchived MemoryState = "archived"
	StateDeleted  MemoryState = "deleted"
)

// Memory represents a piece of shared knowledge stored in a space.
type Memory struct {
	ID         string          `json:"id"`
	Content    string          `json:"content"`
	MemoryType MemoryType      `json:"memory_type"`
	Source     string          `json:"source,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Embedding  []float32       `json:"-"`

	AgentID      string `json:"agent_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	UpdatedBy    string `json:"updated_by,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`

	State     MemoryState `json:"state"`
	Version   int         `json:"version"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`

	Score *float64 `json:"score,omitempty"`

	// Age is a human-readable relative time computed at response time (e.g., "5 minutes ago").
	// Not stored in the database.
	Age string `json:"age,omitempty"`

	// History contains predecessor memories that this memory superseded.
	// Only populated when include_history=true query param is set.
	History []Memory `json:"history,omitempty"`
}

type AuthInfo struct {
	AgentName string

	// Dedicated-cluster model (non-empty when using tenant token)
	TenantID string
	TenantDB *sql.DB
}

// MemoryFilter encapsulates search/list query parameters.
type MemoryFilter struct {
	Query      string
	Tags       []string
	Source     string
	State      string
	MemoryType string
	AgentID    string
	SessionID  string
	Limit      int
	Offset     int
	MinScore   float64 // minimum cosine similarity for vector results; 0 = use default (0.3); -1 = disabled (return all)

	// IncludeHistory indicates whether to include the predecessor chain for each memory.
	// When true, memories that supersede archived ones will include their history.
	IncludeHistory bool
}

// TenantStatus represents the lifecycle status of a tenant.
type TenantStatus string

const (
	TenantProvisioning TenantStatus = "provisioning"
	TenantActive       TenantStatus = "active"
	TenantSuspended    TenantStatus = "suspended"
	TenantDeleted      TenantStatus = "deleted"
)

// Tenant represents a provisioned customer with a dedicated database.
type Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Connection info (never exposed in API responses)
	DBHost     string `json:"-"`
	DBPort     int    `json:"-"`
	DBUser     string `json:"-"`
	DBPassword string `json:"-"`
	DBName     string `json:"-"`
	DBTLS      bool   `json:"-"`

	// Provisioning metadata
	Provider       string     `json:"provider"`
	ClusterID      string     `json:"cluster_id,omitempty"`
	ClaimURL       string     `json:"-"`
	ClaimExpiresAt *time.Time `json:"-"`

	// Lifecycle
	Status        TenantStatus `json:"status"`
	SchemaVersion int          `json:"schema_version"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	DeletedAt     *time.Time   `json:"-"`
}

// DSNForBackend builds a connection string for the specified backend.
// backend must be "postgres", "db9", or "tidb" (MySQL-compatible); empty string panics.
func (t *Tenant) DSNForBackend(backend string) string {
	if backend == "" {
		panic("DSNForBackend: backend must be specified explicitly (\"postgres\", \"db9\", or \"tidb\")")
	}
	switch backend {
	case "postgres", "db9":
		sslmode := "disable"
		if t.DBTLS {
			sslmode = "require"
		}
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
			t.DBUser, t.DBPassword, t.DBHost, t.DBPort, t.DBName, sslmode)
	default:
		// MySQL/TiDB format
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true",
			t.DBUser, t.DBPassword, t.DBHost, t.DBPort, t.DBName)
		if t.DBTLS {
			dsn += "&tls=true"
		}
		return dsn
	}
}

// TenantInfo describes tenant metadata.
type TenantInfo struct {
	TenantID    string       `json:"tenant_id"`
	Name        string       `json:"name"`
	Status      TenantStatus `json:"status"`
	Provider    string       `json:"provider"`
	MemoryCount int          `json:"memory_count"`
	CreatedAt   time.Time    `json:"created_at"`
}

// FormatAge returns a human-readable relative time string based on the given time.
// Examples: "5 minutes ago", "2 hours ago", "3 days ago", "2 weeks ago", "6 months ago", "1 year ago"
func FormatAge(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	case d < 30*24*time.Hour:
		weeks := int(d.Hours() / (24 * 7))
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	case d < 365*24*time.Hour:
		months := int(d.Hours() / (24 * 30))
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	default:
		years := int(d.Hours() / (24 * 365))
		if years == 1 {
			return "1 year ago"
		}
		return fmt.Sprintf("%d years ago", years)
	}
}
