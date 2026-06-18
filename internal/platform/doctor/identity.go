package doctor

import (
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// RepoIdentity describes one registered repo's resolved identity tier and
// whether that tier converges - i.e. whether two contributors indexing the
// same upstream resolve to the same repo_id. Only the
// module-hostpath tier converges AND is globally unique; everything below it
// is local-stable but unsafe to merge into a shared graph DB.
type RepoIdentity struct {
	RepoID    string `json:"repo_id"`
	RootPath  string `json:"root_path"`
	Tier      string `json:"tier"`
	Converges bool   `json:"converges"`
}

// IdentityReport is the result of CheckIdentityTiers.
//
//	Status "healthy" - every registered repo resolved to a converging tier
//	  (or there are no repos / no repos table)
//	Status "degraded" - at least one repo sits on a non-converging tier; its
//	  node_ids will NOT match another contributor indexing the same upstream.
//	  Fine for single-user use; a warning only for the shared-DB goal, so this
//	  status is advisory and does NOT promote the doctor `status` rollup.
//	Status "broken" - the DB could not be opened/pinged/queried (e.g. a
//	  tamper-aborted DB). Reported, never os.Exit.
type IdentityReport struct {
	Repos         []RepoIdentity `json:"repos"`
	NonConverging int            `json:"non_converging"`
	Status        health.Status  `json:"status"`
}

// CheckIdentityTiers opens the SQLite DB at dbPath read-only and classifies
// each registered repo by whether its stored identity_tier converges per
// It warns (degraded) on any non-converging tier so an operator
// preparing to share a graph DB can see which repos would collide or diverge.
// It deliberately opens the raw read-only DSN (like CheckPostPromotionQueue)
// rather than sqlite.OpenWithOptions: the latter runs the migration integrity
// check and os.Exit(78)s on a tampered DB, which a diagnostic must never do.
// A broken DB therefore surfaces as Status "broken" with a nil error.
func CheckIdentityTiers(dbPath string) (IdentityReport, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=1000", dbPath)
	db, err := sql.Open(sqldriver.Name, dsn)
	if err != nil {
		return IdentityReport{Status: health.StatusBroken}, nil
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return IdentityReport{Status: health.StatusBroken}, nil
	}

	// Some test fixtures (and a pre-0018 DB) lack the repos table or the
	// identity_tier column. Absence of the table means there is nothing to
	// classify - report healthy/empty rather than broken.
	var present int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='repos'`,
	).Scan(&present); err != nil {
		return IdentityReport{Status: health.StatusBroken}, nil
	}
	if present == 0 {
		return IdentityReport{Status: health.StatusHealthy}, nil
	}

	repos, err := queryRepoIdentities(db)
	if err != nil {
		return IdentityReport{Status: health.StatusBroken}, nil
	}

	nonConverging := 0
	for _, r := range repos {
		if !r.Converges {
			nonConverging++
		}
	}

	status := health.StatusHealthy
	if nonConverging > 0 {
		status = health.StatusDegraded
	}

	return IdentityReport{
		Repos:         repos,
		NonConverging: nonConverging,
		Status:        status,
	}, nil
}

// queryRepoIdentities reads every repo's stored tier and classifies convergence
// through repo.IdentityTier.Converges - never by hardcoding the tier string, so
// adding/renaming a tier ripples through exactly one method.
func queryRepoIdentities(db *sql.DB) ([]RepoIdentity, error) {
	rows, err := db.Query(
		`SELECT repo_id, root_path, COALESCE(identity_tier, '') FROM repos`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RepoIdentity
	for rows.Next() {
		var r RepoIdentity
		if err := rows.Scan(&r.RepoID, &r.RootPath, &r.Tier); err != nil {
			return nil, err
		}
		r.Converges = repo.IdentityTier(r.Tier).Converges()
		out = append(out, r)
	}
	return out, rows.Err()
}
