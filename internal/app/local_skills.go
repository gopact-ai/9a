package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/projection"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/workspace"
	"gopkg.in/yaml.v3"
)

const localSkillProtocol = "skill"
const localSkillProviderKind = "workspace-skill"

func (a *App) syncAttachedLocalSkills(ctx context.Context, identity string) error {
	workspaces, err := a.projections.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, item := range workspaces {
		status, statusErr := a.projections.Status(ctx, item.Root)
		if statusErr != nil {
			return fmt.Errorf("sync local Skills in %s: %w", item.Root, statusErr)
		}
		if syncErr := a.syncLocalSkills(ctx, identity, status); syncErr != nil {
			return fmt.Errorf("sync local Skills in %s: %w", item.Root, syncErr)
		}
	}
	return nil
}

func (a *App) syncLocalSkills(ctx context.Context, identity string, status projection.Status) error {
	w := status.Workspace
	p, next, err := scanLocalSkills(status)
	if err != nil {
		return err
	}
	repo := catalog.New(a.db)
	all, err := repo.ListCapabilities(ctx)
	if err != nil {
		return err
	}
	current := make([]capability.Capability, 0, len(next))
	for _, item := range all {
		if item.Source.Protocol == localSkillProtocol && item.Source.Provider == w.ID {
			current = append(current, item)
		}
	}
	if len(next) == 0 {
		if len(current) == 0 {
			return nil
		}
		return repo.DeleteProvider(ctx, p.ID)
	}
	if !sameCapabilities(current, next) {
		if _, err = repo.ReplaceProviderCapabilities(ctx, p, next); err != nil {
			return err
		}
	}
	for _, item := range next {
		if _, err = a.az.GrantIfAbsent(ctx, identity, item.ID, authz.Read); err != nil {
			return err
		}
	}
	return nil
}

func scanLocalSkills(status projection.Status) (provider.Provider, []capability.Capability, error) {
	w := status.Workspace
	p := localSkillProvider(w)
	entries, err := os.ReadDir(w.SkillsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil, nil
	}
	if err != nil {
		return p, nil, err
	}
	managed := make(map[string]struct{}, len(status.Skills))
	for _, item := range status.Skills {
		if filepath.Clean(item.TargetRoot) == filepath.Clean(w.SkillsRoot) {
			managed[item.TargetName] = struct{}{}
		}
	}
	capabilities := make([]capability.Capability, 0, len(entries))
	for _, entry := range entries {
		if _, owned := managed[entry.Name()]; owned {
			continue
		}
		root := filepath.Join(w.SkillsRoot, entry.Name())
		info, statErr := os.Stat(root)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return p, nil, statErr
		}
		if !info.IsDir() {
			continue
		}
		if _, markerErr := os.Stat(filepath.Join(root, ".ninea-owned.json")); markerErr == nil {
			continue
		} else if !errors.Is(markerErr, os.ErrNotExist) {
			return p, nil, markerErr
		}
		data, readErr := os.ReadFile(filepath.Join(root, "SKILL.md"))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return p, nil, readErr
		}
		name, description := localSkillMetadata(data, entry.Name())
		digest, digestErr := localSkillDigest(root)
		if digestErr != nil {
			return p, nil, digestErr
		}
		capabilities = append(capabilities, capability.Capability{
			ID:          localSkillID(w.ID, entry.Name()),
			Kind:        "skill.local",
			Name:        name,
			Description: description,
			Source: capability.Source{
				Protocol:     localSkillProtocol,
				Provider:     w.ID,
				UpstreamName: entry.Name(),
			},
			Input:       capability.Contract{Mode: "skill"},
			Output:      capability.Contract{Mode: "skill"},
			Tags:        []string{"local", "skill", entry.Name()},
			RawMetadata: digest,
		})
	}
	return p, capabilities, nil
}

func localSkillID(workspaceID, name string) string {
	id := capability.StableID(localSkillProtocol, workspaceID, name)
	if capability.Slug(name) == name {
		return id
	}
	sum := sha256.Sum256([]byte(name))
	return id + "-" + hex.EncodeToString(sum[:4])
}

func localSkillProvider(w workspace.Workspace) provider.Provider {
	return provider.Provider{
		ID:       localSkillProtocol + "/" + w.ID,
		Protocol: localSkillProtocol,
		Name:     w.ID,
		Endpoint: w.SkillsRoot,
		Config: map[string]string{
			"source_kind":    localSkillProviderKind,
			"workspace_root": w.Root,
		},
	}
}

func (a *App) removeLocalSkills(ctx context.Context, w workspace.Workspace) error {
	repo := catalog.New(a.db)
	items, err := repo.ListCapabilities(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Source.Protocol == localSkillProtocol && item.Source.Provider == w.ID {
			return repo.DeleteProviderPreservingACL(ctx, localSkillProvider(w).ID)
		}
	}
	return nil
}

func localSkillMetadata(data []byte, fallback string) (string, string) {
	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				_ = yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &metadata)
				break
			}
		}
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	if metadata.Name == "" {
		metadata.Name = fallback
	}
	if metadata.Description == "" {
		metadata.Description = "Local Skill " + metadata.Name
	}
	return metadata.Name, metadata.Description
}

func localSkillDigest(root string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	// ponytail: top-level Skill symlinks are followed; follow nested directory
	// symlinks only if real-world Skill layouts need it.
	err = filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(resolved, path)
		if relErr != nil {
			return relErr
		}
		_, _ = io.WriteString(hash, filepath.ToSlash(relative))
		_, _ = hash.Write([]byte{0})
		if entry.Type().IsRegular() {
			file, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		} else if entry.Type()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			_, _ = io.WriteString(hash, target)
		}
		_, _ = hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return []byte(hex.EncodeToString(hash.Sum(nil))), nil
}
