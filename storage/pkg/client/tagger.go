package client

import (
	"context"
	"fmt"
	"strings"
)

// Tagger is the interface for a tagging engine client.
type Tagger interface {
	// Versions returns the versions the tagging engine serves, each "<implementation>" or
	// "<implementation>:<model>", for example "grep" or "decisions/openai:gpt-6-luna". A tagger that
	// switches between several serves one per tagger.
	Versions(ctx context.Context) ([]string, error)
	// Tag evaluates tags for an object. taggerVersion is the version the object's collection is tagged
	// with: the engine refuses with a *VersionMismatchError when it does not serve it.
	Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error)
}

// VersionMismatchError is returned by Tag when the engine does not serve the version that was asked for.
type VersionMismatchError struct {
	Expected string   // the version the collection is tagged with
	Running  []string // the versions the engine serves
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("tagger version mismatch: the collection expects %q, the tagging engine serves %q", e.Expected, strings.Join(e.Running, ", "))
}
