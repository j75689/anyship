package spec

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
)

// JSONSchema returns a JSON Schema for anyship.yaml, so editors can validate
// and autocomplete it (see the yaml-language-server comment Marshal writes).
// Cross-field rules (unknown resources and the like) are only enforced by
// Validate.
func JSONSchema() ([]byte, error) {
	r := &jsonschema.Reflector{ExpandedStruct: true}
	schema := r.Reflect(&Manifest{})
	// No "$id": the schema travels as a file, not as a URL, and every "$ref"
	// in it is a local fragment. The reflector's default would be a GitHub
	// address nothing serves.
	schema.ID = jsonschema.EmptyID
	schema.Title = "anyship deploy spec"
	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// JSONSchemaExtend adds the constraints struct tags can't express.
func (Manifest) JSONSchemaExtend(s *jsonschema.Schema) {
	if v, ok := s.Properties.Get("apiVersion"); ok {
		v.Const = APIVersion
	}
	if k, ok := s.Properties.Get("kind"); ok {
		k.Const = Kind
	}
}

func (Metadata) JSONSchemaExtend(s *jsonschema.Schema) {
	if name, ok := s.Properties.Get("name"); ok {
		name.Pattern = NamePattern
	}
}

func (Spec) JSONSchemaExtend(s *jsonschema.Schema) {
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
