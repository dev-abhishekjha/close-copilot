package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/abhishekjha/close-copilot/internal/seed"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: record not found")

// Company represents a company registered in the Close Copilot system.
type Company struct {
	ID         string `json:"id"`
	ERPCompany string `json:"erp_company"`
	GSTIN      string `json:"gstin"`
}

// UpsertCompany inserts or updates a company row.
func (s *Store) UpsertCompany(ctx context.Context, c Company) error {
	const query = `
		INSERT INTO companies (id, erp_company, gstin)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET
			erp_company = EXCLUDED.erp_company,
			gstin = EXCLUDED.gstin;
	`
	_, err := s.pool.Exec(ctx, query, c.ID, c.ERPCompany, c.GSTIN)
	if err != nil {
		return fmt.Errorf("store: upsert company %s: %w", c.ID, err)
	}
	return nil
}

// GetCompany retrieves a company by its ID.
func (s *Store) GetCompany(ctx context.Context, id string) (Company, error) {
	const query = `
		SELECT id, erp_company, gstin
		FROM companies
		WHERE id = $1;
	`
	var c Company
	err := s.pool.QueryRow(ctx, query, id).Scan(&c.ID, &c.ERPCompany, &c.GSTIN)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Company{}, fmt.Errorf("%w: company %s", ErrNotFound, id)
		}
		return Company{}, fmt.Errorf("store: get company %s: %w", id, err)
	}
	return c, nil
}

// ListCompanies retrieves all companies ordered by ID.
func (s *Store) ListCompanies(ctx context.Context) ([]Company, error) {
	const query = `
		SELECT id, erp_company, gstin
		FROM companies
		ORDER BY id ASC;
	`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list companies: %w", err)
	}
	defer rows.Close()

	var out []Company
	for rows.Next() {
		var c Company
		if err := rows.Scan(&c.ID, &c.ERPCompany, &c.GSTIN); err != nil {
			return nil, fmt.Errorf("store: scan company: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate companies: %w", err)
	}
	return out, nil
}

// SeedCompaniesFromProfiles seeds the companies table from parsed seed.Profiles.
func (s *Store) SeedCompaniesFromProfiles(ctx context.Context, profiles []seed.Profile) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		const query = `
			INSERT INTO companies (id, erp_company, gstin)
			VALUES ($1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET
				erp_company = EXCLUDED.erp_company,
				gstin = EXCLUDED.gstin;
		`
		for _, p := range profiles {
			gstin, err := p.GSTIN()
			if err != nil {
				return fmt.Errorf("store: company %s gstin: %w", p.ID, err)
			}
			if _, err := tx.Exec(ctx, query, p.ID, p.ERPCompany, gstin); err != nil {
				return fmt.Errorf("store: seed company %s: %w", p.ID, err)
			}
		}
		return nil
	})
}

// SeedCompaniesFromDir loads profile YAMLs from a directory and upserts them.
func (s *Store) SeedCompaniesFromDir(ctx context.Context, dir string) error {
	profiles, err := seed.LoadProfiles(dir)
	if err != nil {
		return fmt.Errorf("store: load profiles from %s: %w", dir, err)
	}
	return s.SeedCompaniesFromProfiles(ctx, profiles)
}
