package registry

import (
	"fmt"

	"github.com/gobwas/glob"
	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// GlobAttr provides a YAML handler for glob.Glob so the type can be parsed from YAML or environment variables
type GlobAttr struct {
	// str is kept for debugging/printing purposes
	str  string
	glob glob.Glob
}

func (GlobAttr) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Description: "Glob pattern to match against the attribute value",
		Format:      "glob",
		Examples:    []any{"app-*", "service-??", "prod-*-db"},
	}
}

func NewGlob(pattern string) GlobAttr {
	return GlobAttr{str: pattern, glob: glob.MustCompile(pattern)}
}

func (p *GlobAttr) IsSet() bool {
	return p.glob != nil
}

func (p *GlobAttr) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("GlobAttr: unexpected YAML node kind %d", value.Kind)
	}
	if len(value.Value) == 0 {
		p.glob = nil
		return nil
	}

	re, err := glob.Compile(value.Value)
	if err != nil {
		return fmt.Errorf("invalid regular expression in node %s: %w", value.Tag, err)
	}
	p.str = value.Value
	p.glob = re
	return nil
}

func (p GlobAttr) MarshalYAML() (any, error) {
	return p.str, nil
}

func (p *GlobAttr) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		p.glob = nil
		return nil
	}
	re, err := glob.Compile(string(text))
	if err != nil {
		return fmt.Errorf("invalid regular expression %q: %w", string(text), err)
	}
	p.glob = re
	return nil
}

func (p *GlobAttr) MatchString(input string) bool {
	// no glob means "empty glob", so anything will match it
	if p.glob == nil {
		return true
	}
	return p.glob.Match(input)
}
