// Package company holds the synthetic company profiles
// (config/companies/<id>.yaml) and the check rules (config/rules.yaml):
// their types, loading and strict validation, and the synthetic PAN and
// GSTIN helpers the profiles derive their identifiers from.
//
// It has no dependency on the ERPNext client, the books server or the
// seeder, so the checks, the store and the agent can read profiles and
// rules. internal/seed keeps type aliases and forwarding functions for
// everything here.
//
// Built in CC-601a.
package company
