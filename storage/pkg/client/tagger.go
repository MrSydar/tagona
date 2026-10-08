package client

import (
	"context"
	"fmt"
)

// Tagger is the interface for a tagging engine client.
type Tagger interface {
	// Version returns the version of the tagging engine: "<implementation>" or
	// "<implementation>:<model>", for example "grep" or "decisions/openai:gpt-6-luna".
	Version(ctx context.Context) (string, error)
	// Tag evaluates tags for an object. taggerVersion is the version the object's collection is tagged
	// with: the engine refuses with a *VersionMismatchError when it runs another one.
	Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error)
}

// VersionMismatchError is returned by Tag when the engine's version is not the one that was asked for.
type VersionMismatchError struct {
	Expected string // the version the collection is tagged with
	Running  string // the version the engine runs
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("tagger version mismatch: the collection expects %q, the tagging engine runs %q", e.Expected, e.Running)
}
