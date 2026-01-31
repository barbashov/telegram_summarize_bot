package storage

import "database/sql"

// NullString is a small helper to construct sql.NullString values in tests
// and calling code without repeating the struct literal.
func NullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: s, Valid: true}
}
