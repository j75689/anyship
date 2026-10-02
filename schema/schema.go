// Package schema carries the JSON Schema for anyship.yaml inside the binary.
// The repository is private, so no URL serves the schema to an editor; anyship
// hands out this copy instead, with `anyship schema` or `anyship init`.
package schema

import _ "embed"

// Filename is the name anyship writes the schema under.
const Filename = "anyship.schema.json"

// JSON is schema/anyship.schema.json as of the build. spec.JSONSchema()
// regenerates it from the Go types; TestEmbeddedSchemaMatchesTheFile and
// spec.TestCheckedInJSONSchemaIsCurrent keep the three in step.
//
//go:embed anyship.schema.json
var JSON []byte
