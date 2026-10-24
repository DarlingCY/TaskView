package main

import (
	"database/sql"
	"time"

	"taskview/internal/db"
)

type Store = db.Store
type Task = db.Task
type Project = db.Project
type Stats = db.Stats
type ListOptions = db.ListOptions
type ValidationError = db.ValidationError

var (
	Statuses        = db.Statuses
	Priorities      = db.Priorities
	DefaultStatus   = db.DefaultStatus
	DefaultPriority = db.DefaultPriority
	MaxListLimit    = db.MaxListLimit
	MinListLimit    = db.MinListLimit
	DefaultLimit    = db.DefaultLimit
	BusyTimeoutMS   = db.BusyTimeoutMS
	MaxLineNumber   = db.MaxLineNumber

	schema      = db.Schema
	taskColumns = db.TaskColumns
)

func IsValidationError(err error) bool { return db.IsValidationError(err) }
func OpenStore(filename string) (*Store, error) { return db.OpenStore(filename) }
func DataDir() (string, error) { return db.DataDir() }
func DefaultDBPath() (string, error) { return db.DefaultDBPath() }
func NormalizeProjectPath(input string) (string, error) { return db.NormalizeProjectPath(input) }
func ProjectNameFromPath(path string) string { return db.ProjectNameFromPath(path) }
func ParseLineRange(input string) (*int64, *int64, error) { return db.ParseLineRange(input) }

func newValidationError(format string, args ...any) *ValidationError {
	return db.NewValidationError(format, args...)
}
func canonicalize(data map[string]any) map[string]any {
	return db.Canonicalize(data)
}
func requireString(value any, field string) (string, bool, error) {
	return db.RequireString(value, field)
}

func formatTaskID(prefix string, seq int) string {
	return db.FormatTaskID(prefix, seq)
}
func taskIDPrefix(now time.Time) string {
	return db.TaskIDPrefix(now)
}
func tableHasColumn(dbConn *sql.DB, table, column string) (bool, error) {
	return db.TableHasColumn(dbConn, table, column)
}
func projectPathClause(column string) string {
	return db.ProjectPathClause(column)
}
