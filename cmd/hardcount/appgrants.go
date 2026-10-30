package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// appTableNeeds lists the tables where the app role holds less than full
// read-write by design (the 00180 baseline's grants): the audit log is
// append-only, lease rows are never deleted, and leader_status is read-only.
// Every other table needs SELECT, INSERT, UPDATE and DELETE.
var appTableNeeds = map[string][]string{
	"audit_log":     {"SELECT", "INSERT"},
	"leader_leases": {"SELECT", "INSERT", "UPDATE"},
	"leader_status": {"SELECT"},
}

var fullTableNeeds = []string{"SELECT", "INSERT", "UPDATE", "DELETE"}

// missingAppGrants lists what the pool's role lacks on the tables and
// sequences in the public schema, as GRANT statements an admin can run
// ("GRANT INSERT ON public.webhook_outbox TO hardcount_app"). A database
// created without the role setup in docker/init.sql or
// deploy/compose/postgres-init.sh (the default privileges) booted, then
// failed requests with 500s and logged "permission denied" from the outbox
// workers (#1317). Partitions are read through their parent, and goose's
// own table belongs to the migrator, so neither is checked.
func missingAppGrants(pool *sql.DB) (role string, missing []string, err error) {
	if err := pool.QueryRow(`SELECT current_user`).Scan(&role); err != nil {
		return "", nil, err
	}
	rows, err := pool.Query(`
		SELECT c.relname, c.relkind = 'S',
		       has_table_privilege(c.oid, 'SELECT'), has_table_privilege(c.oid, 'INSERT'),
		       has_table_privilege(c.oid, 'UPDATE'), has_table_privilege(c.oid, 'DELETE'),
		       c.relkind = 'S' AND has_sequence_privilege(c.oid, 'USAGE')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'S')
		  AND NOT c.relispartition AND c.relname <> 'goose_db_version'
		ORDER BY c.relname`)
	if err != nil {
		return role, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			name               string
			isSeq              bool
			sel, ins, upd, del bool
			usage              bool
		)
		if err := rows.Scan(&name, &isSeq, &sel, &ins, &upd, &del, &usage); err != nil {
			return role, nil, err
		}
		if isSeq {
			if !usage {
				missing = append(missing, fmt.Sprintf("GRANT USAGE ON SEQUENCE public.%s TO %s", name, role))
			}
			continue
		}
		have := map[string]bool{"SELECT": sel, "INSERT": ins, "UPDATE": upd, "DELETE": del}
		needs, ok := appTableNeeds[name]
		if !ok {
			needs = fullTableNeeds
		}
		var lack []string
		for _, p := range needs {
			if !have[p] {
				lack = append(lack, p)
			}
		}
		if len(lack) > 0 {
			missing = append(missing, fmt.Sprintf("GRANT %s ON public.%s TO %s", strings.Join(lack, ", "), name, role))
		}
	}
	return role, missing, rows.Err()
}
