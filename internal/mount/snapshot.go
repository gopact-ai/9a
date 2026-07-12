package mount

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type Snapshot struct {
	LogicalID, Name, Version string
	CatalogRevision          int64
	Files                    []File
	Digest                   string
}

type Attachment struct{ WorkspaceID, LogicalID, Target string }
type InspectionState string

const (
	InspectionHealthy  InspectionState = "healthy"
	InspectionMissing  InspectionState = "missing"
	InspectionTampered InspectionState = "tampered"
)

type Inspection struct {
	State  InspectionState
	Reason string
}

func NewSnapshot(logicalID, name, version string, revision int64, files []File) (Snapshot, error) {
	if logicalID == "" || name == "" {
		return Snapshot{}, fmt.Errorf("snapshot identity is required")
	}
	copyFiles := make([]File, len(files))
	copy(copyFiles, files)
	seen := map[string]bool{}
	for i := range copyFiles {
		clean := filepath.ToSlash(filepath.Clean(copyFiles[i].Path))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || seen[clean] {
			return Snapshot{}, fmt.Errorf("unsafe or duplicate snapshot path %q", copyFiles[i].Path)
		}
		seen[clean] = true
		copyFiles[i].Path = clean
		if copyFiles[i].Mode&0o111 != 0 {
			copyFiles[i].Mode = 0o555
		} else {
			copyFiles[i].Mode = 0o444
		}
		copyFiles[i].Data = append([]byte(nil), copyFiles[i].Data...)
	}
	sort.Slice(copyFiles, func(i, j int) bool { return copyFiles[i].Path < copyFiles[j].Path })
	h := sha256.New()
	meta, _ := json.Marshal([]any{logicalID, name, version, revision})
	h.Write(meta)
	for _, f := range copyFiles {
		h.Write([]byte{0})
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(fmt.Sprintf("%o", f.Mode)))
		h.Write([]byte{0})
		h.Write(f.Data)
	}
	return Snapshot{LogicalID: logicalID, Name: name, Version: version, CatalogRevision: revision, Files: copyFiles, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}
