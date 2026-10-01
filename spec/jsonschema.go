package spec

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
)

// JSONSchema returns a JSON Schema for anyship.json, so editors can validate
// and autocomplete it via "$schema". Cross-field rules (unknown resources and
// the like) are only enforced by Validate.
func JSONSchema() ([]byte, error) {
	r := &jsonschema.Reflector{ExpandedStruct: true}
	schema := r.Reflect(&Spec{})
	schema.ID = "https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json"
	schema.Title = "anyship deploy spec"
	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// JSONSchemaExtend adds the constraints struct tags can't express.
func (Spec) JSONSchemaExtend(s *jsonschema.Schema) {
	if version, ok := s.Properties.Get("version"); ok {
		version.Const = Version
	}
	if name, ok := s.Properties.Get("name"); ok {
		name.Pattern = NamePattern
	}
	for prop, pattern := range map[string]string{"services": NamePattern, "resources": NamePattern, "secrets": SecretNamePattern} {
		if p, ok := s.Properties.Get(prop); ok {
			p.PropertyNames = &jsonschema.Schema{Pattern: pattern}
		}
	}
}

func (Volume) JSONSchemaExtend(s *jsonschema.Schema) {
	if name, ok := s.Properties.Get("name"); ok {
		name.Pattern = NamePattern
	}
	if size, ok := s.Properties.Get("size"); ok {
		size.Pattern = SizePattern
	}
	if mount, ok := s.Properties.Get("mountPath"); ok {
		mount.Pattern = "^/"
	}
}
