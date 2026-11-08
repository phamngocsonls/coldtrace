// Package aiagent — small helpers (kept out of the main file for clarity).
package aiagent

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// opsFindSort returns a find-options builder sorting newest-first by at/createdAt.
func opsFindSort() *options.FindOptionsBuilder {
	return options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}})
}

// truncateRunes cuts s to at most max characters without splitting a
// multi-byte rune in half (byte slicing alone can corrupt UTF-8 text).
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
