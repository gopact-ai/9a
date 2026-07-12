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
)

type Workspace struct {
	ID, Root, SkillsRoot string
	Policy               BackendPolicy
	Backend              Backend
	State                State
	FallbackReason       string
	Format               int
	CreatedAt, UpdatedAt time.Time
}

type ManagedSkill struct {
	WorkspaceID, LogicalID, TargetRoot, TargetName, SourceKind, SourceID string
	CatalogRevision                                                      int64
	SkillVersion, Digest, MountState                                     string
	UpdatedAt                                                            time.Time
}
