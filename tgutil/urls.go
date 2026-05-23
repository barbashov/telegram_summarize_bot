// Package tgutil provides small helpers for working with Telegram message data.
package tgutil

import (
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/mymmrac/telego"
)

// bareURLRe matches bare http(s) URLs in plain text. Trailing punctuation is
// trimmed separately.
var bareURLRe = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `)\]]+`)

// ExtractURLsFromText finds bare http(s) URLs in plain text, in order and
// de-duplicated, up to limit (limit <= 0 means no limit). Used for stored
// messages that have no entity metadata.
//
// Limitation: it only recovers URLs written out literally. Markdown-style
// "text_link" URLs (where the visible text differs from the href) are NOT
// recoverable from stored text, since the href was never persisted.
func ExtractURLsFromText(text string, limit int) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, m := range bareURLRe.FindAllString(text, -1) {
		u := strings.TrimRight(m, ".,;:!?")
		// Drop an unbalanced trailing ")" (common when a URL is in parentheses).
		if strings.HasSuffix(u, ")") && strings.Count(u, "(") < strings.Count(u, ")") {
			u = strings.TrimRight(u, ")")
		}
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ExtractURLs returns up to max URLs found in a Telegram message's entities, in
// order of appearance and de-duplicated. It handles "url" entities (where the
// URL text lives in the message body at the entity's offset/length) and
// "text_link" entities (where the URL is carried on the entity itself).
//
// Telegram entity offsets and lengths are measured in UTF-16 code units, not
// runes or bytes, so the message text is converted to UTF-16 before slicing —
// otherwise URLs following emoji or other non-BMP characters come out shifted.
// limit <= 0 means no limit.
func ExtractURLs(text string, entities []telego.MessageEntity, limit int) []string {
	units := utf16.Encode([]rune(text))
	var out []string
	seen := make(map[string]struct{})

	for _, e := range entities {
		var u string
		switch e.Type {
		case "url":
			start := e.Offset
			end := start + e.Length
			if start < 0 || start > end || end > len(units) {
				continue
			}
			u = string(utf16.Decode(units[start:end]))
		case "text_link":
			u = e.URL
		default:
			continue
		}
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
