package graph

import (
	"context"
	"path/filepath"
)

// Artifact is a file a node produced, for observers (graphweb shows it).
type Artifact struct {
	Name    string `json:"name"`              // short label, e.g. "p03" or "script"
	Path    string `json:"path"`              // absolute path of the file on disk
	Mime    string `json:"mime,omitempty"`    // empty = guessed from the extension
	Caption string `json:"caption,omitempty"` // one line shown under it
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
	ao.Artifact(ctx, sc.info, a)
}
