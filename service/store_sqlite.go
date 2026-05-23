package service

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tareksalem/falak/shared/migrations"
)

// runMigrations applies the service schema via the shared migration runner.
func (s *Store) runMigrations() error {
	runner := migrations.NewRunner("service",
		migrations.WithMigrations(serviceMigrations()...),
		migrations.WithLogger(s.logger))
	return runner.Run(s.db)
}

func serviceMigrations() []migrations.Migration {
	return []migrations.Migration{{
		Version:     1,
		Description: "initial services schema",
		Apply: func(tx *sql.Tx) error {
			const ddl = `
			CREATE TABLE services (
				id                  TEXT PRIMARY KEY,
				cluster_id          TEXT NOT NULL DEFAULT '',
				name                TEXT NOT NULL,
				status              TEXT NOT NULL DEFAULT 'created',
				version             TEXT NOT NULL DEFAULT '1',
				spec_json           TEXT NOT NULL,
				backend_states_json TEXT NOT NULL DEFAULT '[]',
				created_at          INTEGER NOT NULL,
				updated_at          INTEGER NOT NULL
			);
			CREATE INDEX idx_services_name    ON services(name);
			CREATE INDEX idx_services_cluster ON services(cluster_id);
			CREATE INDEX idx_services_status  ON services(status);
			`
			_, err := tx.Exec(ddl)
			return err
		},
	}}
}

func (s *Store) loadAll() error {
	rows, err := s.db.Query(`
		SELECT id, cluster_id, name, status, version,
		       spec_json, backend_states_json, created_at, updated_at
		FROM services`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		svc, err := s.scanRow(rows)
		if err != nil {
			return fmt.Errorf("service: scan row: %w", err)
		}
		s.services[svc.ID] = svc
		s.addCapsuleIndex(svc)
	}
	return rows.Err()
}

func (s *Store) insertDB(svc *Service) error {
	specJSON, statesJSON, err := marshalServiceJSON(svc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO services (
			id, cluster_id, name, status, version,
			spec_json, backend_states_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(svc.ID), svc.ClusterID, svc.Spec.Name, string(svc.Status), svc.Version,
		specJSON, statesJSON,
		svc.CreatedAt.UnixMilli(), svc.UpdatedAt.UnixMilli(),
	)
	return err
}

func (s *Store) updateDB(svc *Service) error {
	specJSON, statesJSON, err := marshalServiceJSON(svc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		UPDATE services SET
			cluster_id = ?, name = ?, status = ?, version = ?,
			spec_json = ?, backend_states_json = ?, updated_at = ?
		WHERE id = ?`,
		svc.ClusterID, svc.Spec.Name, string(svc.Status), svc.Version,
		specJSON, statesJSON, svc.UpdatedAt.UnixMilli(),
		string(svc.ID),
	)
	return err
}

func marshalServiceJSON(svc *Service) (string, string, error) {
	specJSON, err := json.Marshal(svc.Spec)
	if err != nil {
		return "", "", fmt.Errorf("marshal spec: %w", err)
	}
	statesJSON, err := json.Marshal(svc.BackendStates)
	if err != nil {
		return "", "", fmt.Errorf("marshal backend states: %w", err)
	}
	return string(specJSON), string(statesJSON), nil
}

func (s *Store) scanRow(rows *sql.Rows) (*Service, error) {
	var (
		id, clusterID, name, status, version string
		specJSON, statesJSON                 sql.NullString
		createdAt, updatedAt                 int64
	)
	if err := rows.Scan(&id, &clusterID, &name, &status, &version,
		&specJSON, &statesJSON, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	svc := &Service{
		ID:        ServiceID(id),
		ClusterID: clusterID,
		Status:    ServiceStatus(status),
		Version:   version,
		CreatedAt: time.UnixMilli(createdAt),
		UpdatedAt: time.UnixMilli(updatedAt),
	}
	if specJSON.Valid && specJSON.String != "" {
		if err := json.Unmarshal([]byte(specJSON.String), &svc.Spec); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
	}
	if statesJSON.Valid && statesJSON.String != "" {
		if err := json.Unmarshal([]byte(statesJSON.String), &svc.BackendStates); err != nil {
			return nil, fmt.Errorf("unmarshal backend states: %w", err)
		}
	}
	return svc, nil
}
