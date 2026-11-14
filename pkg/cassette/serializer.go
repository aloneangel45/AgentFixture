package cassette

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ToYAML serializes a Cassette into formatted YAML bytes without mutating the caller's Cassette in place.
func ToYAML(c *Cassette) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("cannot serialize nil cassette")
	}
	snapshot := c.Clone()
	snapshot.ComputeMetrics()
	RedactCassette(snapshot)
	return yaml.Marshal(snapshot)
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

// ToJSON serializes a Cassette into indented JSON bytes without mutating the caller's Cassette in place.
func ToJSON(c *Cassette) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("cannot serialize nil cassette")
	}
	snapshot := c.Clone()
	snapshot.ComputeMetrics()
	RedactCassette(snapshot)
	return json.MarshalIndent(snapshot, "", "  ")
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

// Save writes a Cassette atomically to disk via a temporary file + fsync + rename,
// automatically selecting YAML or JSON based on file extension.
func Save(path string, c *Cassette) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("cassette path cannot be empty")
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

	return writeFileAtomic(path, data, 0644)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	tmpFile, err := os.CreateTemp(dir, ".agentfixture-tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write temporary cassette file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to sync temporary cassette file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary cassette file: %w", err)
	}
	_ = os.Chmod(tmpPath, perm)

	if err := os.Rename(tmpPath, path); err != nil {
		if remErr := os.Remove(path); remErr == nil {
			if renErr := os.Rename(tmpPath, path); renErr == nil {
				committed = true
				return nil
			}
		}
		return fmt.Errorf("failed to atomically write cassette %s: %w", path, err)
	}
	committed = true
	return nil
}

// Load reads a Cassette from disk, determining format by file extension.
func Load(path string) (*Cassette, error) {
	c, err := LoadUnvalidated(path)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cassette structure: %w", err)
	}
	return c, nil
}

// LoadUnvalidated reads and unmarshals a Cassette from disk (YAML or JSON) without running Validate().
func LoadUnvalidated(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cassette file %s: %w", path, err)
	}

	var c Cassette
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cassette json: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cassette yaml: %w", err)
		}
	}
	return &c, nil
}
