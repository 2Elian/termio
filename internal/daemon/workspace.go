package daemon

import (
	"errors"
	"fmt"
	"sync"

	"termio/internal/config"
	"termio/internal/protocol"
	"termio/internal/tools"
)

// ErrWorkspaceNotFound is returned when a call references an unknown workspace.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// Workspace is one locally authorized execution context. Sessions share a
// workspace; the workspace is the isolation boundary, not the session.
type Workspace struct {
	ID       string
	Name     string
	Registry *tools.Registry
	Sandbox  *tools.Sandbox
	Sem      chan struct{}
}

// WorkspaceSet holds all workspaces loaded by the daemon.
type WorkspaceSet struct {
	defaultID string
	byID      map[string]*Workspace
	order     []string
	closeOnce sync.Once
}

// NewWorkspaceSet builds a set from explicit workspaces. It is used by tests
// and by callers that construct sandboxes themselves.
func NewWorkspaceSet(defaultID string, workspaces ...*Workspace) (*WorkspaceSet, error) {
	if len(workspaces) == 0 {
		return nil, errors.New("at least one workspace is required")
	}
	set := &WorkspaceSet{defaultID: defaultID, byID: make(map[string]*Workspace, len(workspaces))}
	for _, ws := range workspaces {
		if ws == nil || ws.ID == "" {
			return nil, errors.New("workspace id is required")
		}
		if _, exists := set.byID[ws.ID]; exists {
			return nil, fmt.Errorf("duplicate workspace id %q", ws.ID)
		}
		if ws.Registry == nil {
			return nil, fmt.Errorf("workspace %q has no tool registry", ws.ID)
		}
		if ws.Sem == nil {
			ws.Sem = make(chan struct{}, 1)
		}
		set.byID[ws.ID] = ws
		set.order = append(set.order, ws.ID)
	}
	if set.defaultID == "" {
		set.defaultID = set.order[0]
	}
	if _, ok := set.byID[set.defaultID]; !ok {
		return nil, fmt.Errorf("default workspace %q not found", set.defaultID)
	}
	return set, nil
}

// LoadWorkspaces opens every configured workspace. factory may be nil, in
// which case tools.DefaultRegistry is used.
func LoadWorkspaces(cfg *config.Config, configPath string, factory func(*tools.Sandbox) *tools.Registry) (*WorkspaceSet, error) {
	if factory == nil {
		factory = tools.DefaultRegistry
	}
	appData, err := config.AppDataDir()
	if err != nil {
		return nil, err
	}
	deny := []string{appData}
	if configPath != "" {
		deny = append(deny, configPath)
	}
	if cfg.LogFile != "" {
		deny = append(deny, cfg.LogFile)
	}
	maxConcurrent := cfg.MaxConcurrentCalls
	if maxConcurrent < 1 {
		maxConcurrent = config.DefaultMaxConcurrent
	}

	var opened []*Workspace
	closeAll := func() {
		for _, ws := range opened {
			if ws.Sandbox != nil {
				_ = ws.Sandbox.Close()
			}
		}
	}
	for _, w := range cfg.Workspaces {
		sb, err := tools.NewSandbox(w.Path, w.ExtraRoots, deny...)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("workspace %q: %w", w.ID, err)
		}
		opened = append(opened, &Workspace{
			ID:       w.ID,
			Name:     w.Name,
			Sandbox:  sb,
			Registry: factory(sb),
			Sem:      make(chan struct{}, maxConcurrent),
		})
	}
	set, err := NewWorkspaceSet(cfg.DefaultWorkspaceID, opened...)
	if err != nil {
		closeAll()
		return nil, err
	}
	return set, nil
}

// DefaultID returns the workspace used when a call omits workspaceId.
func (s *WorkspaceSet) DefaultID() string { return s.defaultID }

// Get returns a workspace by ID, or the default when id is empty.
func (s *WorkspaceSet) Get(id string) (*Workspace, error) {
	if id == "" {
		id = s.defaultID
	}
	ws, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceNotFound, id)
	}
	return ws, nil
}

// Default returns the default workspace.
func (s *WorkspaceSet) Default() *Workspace { return s.byID[s.defaultID] }

// List returns workspace metadata for registration and debug endpoints.
func (s *WorkspaceSet) List() []protocol.WorkspaceInfo {
	out := make([]protocol.WorkspaceInfo, 0, len(s.order))
	for _, id := range s.order {
		ws := s.byID[id]
		out = append(out, protocol.WorkspaceInfo{ID: ws.ID, Name: ws.Name})
	}
	return out
}

// Specs returns the union of tool specs across workspaces, deduplicated by
// name in workspace order. Workspaces normally expose the same tools; the
// union keeps tools/list correct if one is configured differently.
func (s *WorkspaceSet) Specs() []protocol.ToolSpec {
	seen := make(map[string]bool)
	var out []protocol.ToolSpec
	for _, id := range s.order {
		for _, spec := range s.byID[id].Registry.Specs() {
			if seen[spec.Name] {
				continue
			}
			seen[spec.Name] = true
			out = append(out, spec)
		}
	}
	return out
}

// Close closes all workspace sandboxes.
func (s *WorkspaceSet) Close() error {
	var errs []error
	s.closeOnce.Do(func() {
		for _, id := range s.order {
			ws := s.byID[id]
			if ws.Sandbox != nil {
				if err := ws.Sandbox.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		}
	})
	return errors.Join(errs...)
}
