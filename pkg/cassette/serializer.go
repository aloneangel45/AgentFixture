package cassette

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ToYAML serializes a Cassette into formatted YAML bytes.
func ToYAML(c *Cassette) ([]byte, error) {
	c.ComputeMetrics()
	RedactCassette(c)
	return yaml.Marshal(c)
}

// FromYAML parses YAML bytes into a Cassette and validates it.
func FromYAML(data []byte) (*Cassette, error) {
	var c Cassette
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cassette yaml: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cassette structure: %w", err)
	}
	return &c, nil
}

// ToJSON serializes a Cassette into indented JSON bytes.
func ToJSON(c *Cassette) ([]byte, error) {
	c.ComputeMetrics()
	RedactCassette(c)
	return json.MarshalIndent(c, "", "  ")
}

// FromJSON parses JSON bytes into a Cassette and validates it.
func FromJSON(data []byte) (*Cassette, error) {
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cassette json: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cassette structure: %w", err)
	}
	return &c, nil
}

// Save writes a Cassette to disk, automatically selecting YAML or JSON based on file extension.
func Save(path string, c *Cassette) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	ext := strings.ToLower(filepath.Ext(path))
	var data []byte
	var err error

	if ext == ".json" {
		data, err = ToJSON(c)
	} else {
		data, err = ToYAML(c)
	}
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// Load reads a Cassette from disk, determining format by file extension.
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cassette file %s: %w", path, err)
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		return FromJSON(data)
	}
	return FromYAML(data)
}
