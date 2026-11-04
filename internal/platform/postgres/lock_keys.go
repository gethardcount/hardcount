package postgres

// Transaction-scoped advisory-lock keys. This is the only advisory lock left
// in the app, because singleton-role leadership is a leader_leases row
// (ADR-114):
//
//   - LockKeyBootstrap serializes first-boot tenant bootstrap, with
//     pg_advisory_xact_lock inside the bootstrap transaction.
//
// Migrations take goose's own session lock (id 4097083626, ADR-125) on a
// dedicated one-connection pool. That key is goose's, so it is not listed
// here.
//
// Do not reuse the numbers of retired keys. An old binary in a mixed fleet
// still takes the per-role session keys 76540001-76540005 and the
// topology-probe key 76540008 for the duration of its tick. v0.8.x migration
// runs take the hybrid migration loop key 76540007.
const (
	LockKeyBootstrap int64 = 76540006
)
