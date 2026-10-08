// Package diagnose gathers what anyship knows about a deployment that fails,
// for an agent to diagnose.
//
// It collects deterministic facts (the spec, plan findings, the target's
// checks, status, logs, generated files) through the adapter contract and
// redacts secrets before any of it leaves the process. anyship calls no model:
// the CLI prints the context and the MCP server returns it, and whatever the
// agent proposes is an edit to anyship.yaml that plan checks like any other.
package diagnose
