package store

import (
	"testing"
)

// The version has to be the real one, not a guess or a zero standing in for
// "could not tell". A control plane that logged "schema version 0" about a
// database it failed to read would be inventing the reassuring answer.
func TestSchemaVersionReportsWhatMigrateApplied(t *testing.T) {
	st := testStore(t)

	v, dirty, err := st.SchemaVersion(testCtx(t))
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if dirty {
		t.Fatalf("the test database is mid-migration at version %d; run make migrate-check", v)
	}
	// Not a hardcoded number, which would need editing with every migration and
	// would fail for the wrong reason when somebody forgot. The property that
	// matters is that it reports something real: the newest migration on disk
	// is applied, so the version must be at least the one that created the
	// table this package's newest tests use.
	if v < 19 {
		t.Fatalf("schema version %d, but access_requests (0019) is in use by these tests", v)
	}
}
