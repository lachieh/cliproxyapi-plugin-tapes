package router

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The README's configuration example is the reference most edits start from, so it must parse.
func TestReadmeExampleConfigIsValid(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(readme), "## Configuration")
	_, block, _ := strings.Cut(rest, "```yaml\n")
	block, _, _ = strings.Cut(block, "```")

	var doc struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatalf("README example is not YAML: %v", err)
	}
	node, ok := doc.Plugins.Configs["lachieh-router"]
	if !ok {
		t.Fatal("README example has no plugins.configs.lachieh-router")
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(raw, NewState(), fakeUsage{})
	if err != nil {
		t.Fatalf("README example config is invalid: %v", err)
	}

	// Non-Claude providers must fall through to the host, not hit the reject at the end.
	res := r.Simulate(SimInput{Provider: "openai-compatible-mock", Candidates: []string{"openai-compatibility:mock:1"}}, weekdayMorning)
	if res.Result != "builtin" {
		t.Errorf("README tree should leave other providers to the host, got %q via %+v", res.Result, res.Path)
	}
}
