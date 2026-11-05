package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Artifact is a file a node produced, for observers (graphweb shows it).
type Artifact struct {
	Name    string `json:"name"`              // short label, e.g. "p03" or "script"
	Path    string `json:"path"`              // absolute path of the file on disk
	Mime    string `json:"mime,omitempty"`    // empty = guessed from the extension
	Caption string `json:"caption,omitempty"` // one line shown under it
	// Data is the absolute path of the JSON this artifact was rendered from
	// (EmitData): Path is the human view (markdown), Data the source. Empty
	// for plain files.
	Data string `json:"data,omitempty"`
}

// ArtifactObserver is the optional Observer extension that receives artifacts.
type ArtifactObserver interface {
	Artifact(ctx context.Context, info StepInfo, a Artifact)
}

// EmitArtifact reports a to the run's observer from inside a node (or a parallel branch).
// It is a no-op outside a run or when the observer does not implement ArtifactObserver.
//
// A relative Path is made absolute and an empty Name becomes the file's base
// name. The file is not read here: observers serve it later, so it must stay
// on disk for as long as the run is being watched.
func EmitArtifact(ctx context.Context, a Artifact) {
	sc, ok := ctx.Value(scopeKey{}).(*scope)
	if !ok {
		return
	}
	ao, ok := sc.obs.(ArtifactObserver)
	if !ok {
		return
	}
	if abs, err := filepath.Abs(a.Path); err == nil {
		a.Path = abs
	}
	if a.Name == "" {
		a.Name = filepath.Base(a.Path)
	}
	if a.Data != "" {
		if abs, err := filepath.Abs(a.Data); err == nil {
			a.Data = abs
		}
	}
	ao.Artifact(ctx, sc.info, a)
}

// EmitData publishes structured node output for people: v is written as
// <dir>/<file>.json, its markdown projection md(v) (when md is non-nil) as
// <dir>/<file>.md, and one artifact is emitted whose Path is the markdown and
// whose Data is the JSON (graphweb shows the markdown rendered, with the JSON
// one click away). Without md the artifact is the JSON itself.
//
// The files are written even outside a run; only the emit is skipped. file is
// a base name without extension ("script_v2").
func EmitData(ctx context.Context, dir, file, name string, v any, md func() string, caption string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("graph: emit data: %w", err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("graph: emit data %q: %w", name, err)
	}
	js := filepath.Join(dir, file+".json")
	if err := os.WriteFile(js, b, 0o644); err != nil {
		return fmt.Errorf("graph: emit data: %w", err)
	}
	a := Artifact{Name: name, Path: js, Mime: "application/json", Caption: caption}
	if md != nil {
		mdPath := filepath.Join(dir, file+".md")
		if err := os.WriteFile(mdPath, []byte(md()), 0o644); err != nil {
			return fmt.Errorf("graph: emit data: %w", err)
		}
		a.Path, a.Mime, a.Data = mdPath, "text/markdown", js
	}
	EmitArtifact(ctx, a)
	return nil
}
