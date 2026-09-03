package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SchemaVersion reports which migration the database is actually at.
//
// It exists because two halves of a deployment are pinned by different
// mechanisms and can drift silently: the server images come from a registry by
// tag, while `migrations/` is bind-mounted off the host filesystem. Upgrade the
// images without pulling the repository and the control plane starts cleanly,
// serves every route that existed before, and fails only on whatever the new
// migration was for — as a 500 from one handler, with nothing in any log
// suggesting the schema is behind.
//
// Reading golang-migrate's own bookkeeping table rather than probing for tables
// we expect: this is the number the tool itself would print, so an operator can
// compare it against `ls migrations/` and get an answer in one step.
//
// dirty means a migration failed partway and the schema is in neither the old
// shape nor the new one. golang-migrate refuses to proceed until somebody
// resolves it by hand, so it is worth saying loudly rather than reporting a
// version that is not really in effect.
func (s *Store) SchemaVersion(ctx context.Context) (version int64, dirty bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The table exists but holds nothing: migrate created it and then
		// applied nothing, which is what an empty database looks like
		// mid-bootstrap.
		return 0, false, nil
	case err != nil:
		// Anything else — including the table not existing at all, on a
		// database migrate has never touched — is reported rather than
		// flattened to zero. A caller logging "schema version 0" about a
		// database it could not read would be inventing the reassuring answer.
		return 0, false, mapErr(err)
	}
	return version, dirty, nil
}
