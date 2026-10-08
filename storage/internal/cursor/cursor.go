package cursor

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Encode creates a cursor from date and id.
func Encode(date time.Time, id string) string {
	slog.Debug("Encode", "date", date.Format(time.RFC3339), "id", id)
	unixMillis := date.UnixNano() / 1_000_000
	raw := fmt.Sprintf("%d|%s", unixMillis, id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// Decode parses a cursor into date millis and id.
func Decode(cursor string) (time.Time, string, error) {
	slog.Debug("Decode", "cursor", cursor)
	if cursor == "" {
		return time.Time{}, "", fmt.Errorf("empty cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor encoding: %w", err)
	}
	parts := strings.SplitN(string(data), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("invalid cursor format")
	}
	unixMillis, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor date: %w", err)
	}
	date := time.Unix(0, unixMillis*1_000_000).UTC()
	return date, parts[1], nil
}

// EncodeKey creates the cursor of a position in a list that is ordered by creation time and id, such as the
// list of collections. Unlike Encode it keeps the time to the microsecond, which is what the database
// stores, and its format cannot be mistaken for an object cursor.
func EncodeKey(created time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("k%d|%s", created.UnixMicro(), id)))
}

// DecodeKey parses a cursor made by EncodeKey.
func DecodeKey(cursor string) (time.Time, string, error) {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor encoding: %w", err)
	}
	micros, id, ok := strings.Cut(string(data), "|")
	if !ok || !strings.HasPrefix(micros, "k") || id == "" {
		return time.Time{}, "", fmt.Errorf("invalid cursor format")
	}
	n, err := strconv.ParseInt(micros[1:], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor date: %w", err)
	}
	return time.UnixMicro(n).UTC(), id, nil
}
