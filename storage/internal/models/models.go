package models

import (
	"time"
)

// Collection represents a collection in the system.
type Collection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// TaggerVersion is the version of the tagger the collection is tagged with, "<implementation>" or
	// "<implementation>:<model>".
	TaggerVersion string    `json:"tagger_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// Object represents object metadata.
type Object struct {
	ID           string     `json:"id"`
	Collection   string     `json:"collection"`
	CollectionID string     `json:"-"`
	Date         time.Time  `json:"date"`
	SizeBytes    int64      `json:"size_bytes"`
	ContentHash  string     `json:"content_hash"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	PayloadKey   string     `json:"payload_key,omitempty"`
	// Metadata is what the uploader attached to the object: string keys and string values. Never null.
	Metadata map[string]string `json:"metadata"`
	// Tags are the scores (0 to 1) of the tags a query asked about, or of all the tags the object has.
	Tags map[string]float64 `json:"tags,omitempty"`
}

// DefaultThreshold is the score at or above which a tag counts as true when a query names no threshold.
const DefaultThreshold = 0.5

// Matches reports whether a tag's score satisfies a wanted value at a threshold: true wants a score of at least
// threshold, false wants one below it.
func Matches(score float64, want bool, threshold float64) bool {
	if want {
		return score >= threshold
	}
	return score < threshold
}

// Forced is the scores a tag set by hand is stored with: 1 for true and 0 for false.
func Forced(tags map[string]bool) map[string]float64 {
	out := make(map[string]float64, len(tags))
	for tag, v := range tags {
		if v {
			out[tag] = 1
		} else {
			out[tag] = 0
		}
	}
	return out
}

// TagStat describes one tag registered in a collection. Tags are sparse: an
// object has a score for a tag only once the tag was evaluated for it.
type TagStat struct {
	Tag string `json:"tag"`
	// TrueCount and FalseCount are the objects the tag is known for with a score of at least 0.5 and below it.
	TrueCount   int64     `json:"true_count"`
	FalseCount  int64     `json:"false_count"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

// CollectionTagsResponse is the response for listing a collection's tags.
type CollectionTagsResponse struct {
	Collection   string    `json:"collection"`
	TotalObjects int64     `json:"total_objects"`
	Tags         []TagStat `json:"tags"`
	Next         string    `json:"next,omitempty"`
}

// ErrorResponse is the standard error response format.
type ErrorResponse struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

// CollectionCreateRequest is the request to create a collection. An omitted tagger_version is the version
// of the tagger that is running now.
type CollectionCreateRequest struct {
	Name          string `json:"name"`
	TaggerVersion string `json:"tagger_version"`
}

// CollectionsListResponse is the response for listing collections.
type CollectionsListResponse struct {
	Collections []Collection `json:"collections"`
	// Next is the cursor of the following page; absent on the last page.
	Next string `json:"next,omitempty"`
}

// ObjectUploadResponse is the response after uploading an object.
type ObjectUploadResponse struct {
	ID          string            `json:"id"`
	Collection  string            `json:"collection"`
	Date        time.Time         `json:"date"`
	SizeBytes   int64             `json:"size_bytes"`
	ContentHash string            `json:"content_hash"`
	Metadata    map[string]string `json:"metadata"`
}

// TagsQueryRequest is the request for querying objects by tags.
type TagsQueryRequest struct {
	// Tags are the tags to match: true wants a score of at least Threshold, false one below it.
	Tags map[string]bool `json:"tags,omitempty"`
	// Threshold is the score a tag needs to count as true; DefaultThreshold when omitted.
	Threshold  *float64    `json:"threshold,omitempty"`
	Date       *DateFilter `json:"date,omitempty"`
	Limit      int         `json:"limit"`
	Cursor     string      `json:"cursor,omitempty"`
	TimeoutMs  int         `json:"timeout_ms,omitempty"`
	BestEffort bool        `json:"best_effort,omitempty"`
	// Evaluate controls whether missing tags are evaluated by the tagging
	// engine. It defaults to true when omitted; when false the query is answered
	// from already-known tags only and the tagger is never called.
	Evaluate *bool `json:"evaluate,omitempty"`
}

// EffectiveThreshold is the threshold of the query: the requested one, or the default.
func (r TagsQueryRequest) EffectiveThreshold() float64 {
	if r.Threshold == nil {
		return DefaultThreshold
	}
	return *r.Threshold
}

// ShouldEvaluate reports whether missing tags may be evaluated (the default).
func (r TagsQueryRequest) ShouldEvaluate() bool {
	return r.Evaluate == nil || *r.Evaluate
}

// TagsQueryResponse is the response for tag queries.
type TagsQueryResponse struct {
	Objects []Object `json:"objects"`
	Next    string   `json:"next,omitempty"`
}

// DateFilter defines date constraints.
type DateFilter struct {
	GT  *time.Time `json:"gt,omitempty"`
	GTE *time.Time `json:"gte,omitempty"`
	LT  *time.Time `json:"lt,omitempty"`
	LTE *time.Time `json:"lte,omitempty"`
	EQ  *time.Time `json:"eq,omitempty"`
}

// TaggingRequest is the request to the tagging engine.
type TaggingRequest struct {
	Collection string   `json:"collection"`
	ObjectID   string   `json:"object_id"`
	Tags       []string `json:"tags"`
}

// TaggingResponse is the response from the tagging engine.
type TaggingResponse struct {
	Tags map[string]float64 `json:"tags"`
}
