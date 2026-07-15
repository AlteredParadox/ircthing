// Package store provides SQLite persistence (WAL mode): messages, networks,
// channels, read markers, and FTS5 search. Schema changes go through
// migrations/ — never mutate schema in place.
package store
