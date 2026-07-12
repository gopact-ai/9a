package workspace

import "time"

type BackendPolicy string
type Backend string
type State string

const (
	PolicyAuto       BackendPolicy = "auto"
	PolicyFUSE       BackendPolicy = "fuse"
	PolicyDirectory  BackendPolicy = "directory"
	BackendFUSE      Backend       = "fuse"
	BackendDirectory Backend       = "directory"
	StateHealthy     State         = "healthy"
	StateFallback    State         = "fallback"
	StateDegraded    State         = "degraded"
	StateTampered    State         = "tampered"
	StateDetached    State         = "detached"
)

type Workspace struct {
	ID             string        `json:"id"`
	Root           string        `json:"root"`
	SkillsRoot     string        `json:"skills_root"`
	Policy         BackendPolicy `json:"policy"`
	Backend        Backend       `json:"backend"`
	State          State         `json:"state"`
	FallbackReason string        `json:"fallback_reason,omitempty"`
	Format         int           `json:"format"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type ManagedSkill struct {
	WorkspaceID     string    `json:"workspace_id"`
	LogicalID       string    `json:"logical_id"`
	TargetRoot      string    `json:"target_root"`
	TargetName      string    `json:"target_name"`
	SourceKind      string    `json:"source_kind"`
	SourceID        string    `json:"source_id"`
	CatalogRevision int64     `json:"catalog_revision"`
	SkillVersion    string    `json:"skill_version"`
	Digest          string    `json:"digest"`
	MountState      string    `json:"mount_state"`
	UpdatedAt       time.Time `json:"updated_at"`
}
