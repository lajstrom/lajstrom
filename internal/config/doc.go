// Package config loads the Lajstrom configuration: defaults, then a YAML file, then LAJSTROM_*
// environment overrides for scalar keys, then secrets named by *Env keys, then validation.
// The keys are documented in docs/configuration.md; the shape is docs/spec/layout.md §6 and an
// example lives in deploy/compose/lajstrom.example.yaml.
package config
