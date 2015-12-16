package main

import (
	"os"

	"github.com/flynn/flynn/pkg/cluster"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/random"

	. "github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-check"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/jackc/pgx"
)

type MigrateSuite struct{}

var _ = Suite(&MigrateSuite{})

// TestMigrateJobStates checks that migrating to ID 9 does not break existing
// job records
func (MigrateSuite) TestMigrateJobStates(c *C) {
	dbname := "controller_migrate_test"
	db := setupTestDB(c, dbname)

	currID := 0
	migrateTo := func(d *postgres.DB, id int) {
		c.Assert((*migrations)[currID:id].Migrate(d), IsNil)
		currID = id
		d.Reset()
	}

	// start from ID 7
	migrateTo(db, 7)

	// test queries
	testQueries := map[string]string{
		"app_insert":       `INSERT INTO apps (app_id, name) VALUES ($1, $2)`,
		"release_insert":   `INSERT INTO releases (release_id) VALUES ($1)`,
		"job_insert":       `INSERT INTO job_cache (job_id, app_id, release_id, state) VALUES ($1, $2, $3, $4)`,
		"job_update_state": `UPDATE job_cache SET state = $2 WHERE job_id = $1`,
	}
	prepareTestQueries := func(conn *pgx.Conn) error {
		for name, sql := range testQueries {
			if _, err := conn.Prepare(name, sql); err != nil {
				c.Fatal(err)
			}
		}
		return nil
	}
	// reconnect to db, preparing test queries
	pgxpool, err := pgx.NewConnPool(pgx.ConnPoolConfig{
		ConnConfig: pgx.ConnConfig{
			Host:     os.Getenv("PGHOST"),
			Database: dbname,
		},
		AfterConnect: prepareTestQueries,
	})
	if err != nil {
		c.Fatal(err)
	}
	db = postgres.New(pgxpool, nil)

	// insert a job
	hostID := "host1"
	uuid := random.UUID()
	jobID := cluster.GenerateJobID(hostID, uuid)
	appID := random.UUID()
	releaseID := random.UUID()
	c.Assert(db.Exec("app_insert", appID, "migrate-app"), IsNil)
	c.Assert(db.Exec("release_insert", releaseID), IsNil)
	c.Assert(db.Exec("job_insert", jobID, appID, releaseID, "up"), IsNil)

	// migrate to 8 and check job states are still constrained
	migrateTo(db, 8)
	err = db.Exec("job_update_state", jobID, "foo")
	c.Assert(err, NotNil)
	if !postgres.IsPostgresCode(err, postgres.ForeignKeyViolation) {
		c.Fatalf("expected postgres foreign key violation, got %s", err)
	}

	// migrate to 9 and check job IDs are correct, pending state is valid
	migrateTo(db, 9)
	var clusterID, dbUUID, dbHostID string
	c.Assert(db.QueryRow("SELECT cluster_id, job_id, host_id FROM job_cache WHERE cluster_id = $1", jobID).Scan(&clusterID, &dbUUID, &dbHostID), IsNil)
	c.Assert(clusterID, Equals, jobID)
	c.Assert(dbUUID, Equals, uuid)
	c.Assert(dbHostID, Equals, hostID)
	c.Assert(db.Exec("job_update_state", uuid, "pending"), IsNil)
}
