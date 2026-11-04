package db

import (
	"fmt"
	"path/filepath"
)

// RecordAuxiliaryRunCheckout records a checkout created by an internal run
// operation, never by an external origin assertion. Existing bindings cannot
// be replaced. Rows share the run's retention and contain no caller identity.
func (d *DB) RecordAuxiliaryRunCheckout(runID, checkout, commonDir string) error {
	if !filepath.IsAbs(checkout) || !filepath.IsAbs(commonDir) {
		return fmt.Errorf("execution checkout and git directory must be absolute")
	}
	result, err := d.sql.Exec(`INSERT INTO run_execution_checkouts (run_id,checkout,git_common_dir) SELECT id,?,? FROM runs WHERE id=? AND caller_origin IS NOT NULL`, checkout, commonDir, runID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("auxiliary checkout requires an attributed run")
	}
	return nil
}

func (d *DB) AuxiliaryRunCheckout(runID, checkout string) (string, error) {
	var common string
	err := d.sql.QueryRow(`SELECT git_common_dir FROM run_execution_checkouts WHERE run_id=? AND checkout=?`, runID, checkout).Scan(&common)
	if err != nil {
		return "", fmt.Errorf("execution reference belongs to a different or unrecorded checkout: %w", err)
	}
	return common, nil
}
