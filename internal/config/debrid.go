package config

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DebridProviderRealDebrid is the only debrid provider v0.2.0 ships.
const DebridProviderRealDebrid = "realdebrid"

// DebridConfig holds the non-secret debrid settings. Credentials never live
// here: they are kept in debrid.json (see DebridPath), owner-readable only.
type DebridConfig struct {
	// Enabled is the "use for downloads" switch. A linked account is used
	// only while it is on, so turning it off restores plain torrent
	// downloads without unlinking.
	Enabled bool `yaml:"enabled"`
	// DeleteAfterDownload removes the cloud copy once the local download
	// has finished. Off by default: the copy stays on the provider.
	DeleteAfterDownload bool `yaml:"delete_after_download"`
	// Provider names the debrid service; only "realdebrid" exists today.
	Provider string `yaml:"provider"`
}

// DefaultDebridConfig is the debrid block a fresh config gets.
func DefaultDebridConfig() DebridConfig {
	return DebridConfig{Enabled: true, Provider: DebridProviderRealDebrid}
}

// WriteDebridEnabled saves debrid.enabled, keeping comments, unrelated keys
// and the file mode. The caller updates its in-memory config after success.
func (c *Config) WriteDebridEnabled(enabled bool) error {
	return c.writeScalar([]string{"debrid", "enabled"}, "!!bool", strconv.FormatBool(enabled))
}

// WriteDebridDeleteAfterDownload saves debrid.delete_after_download the same
// way as WriteDebridEnabled.
func (c *Config) WriteDebridDeleteAfterDownload(on bool) error {
	return c.writeScalar([]string{"debrid", "delete_after_download"}, "!!bool", strconv.FormatBool(on))
}

// writeScalar sets one scalar at keys (a path of nested mapping keys),
// creating missing mappings, and rewrites config.yaml atomically. A missing
// config is seeded from the defaults. It never follows symlinks and refuses
// malformed YAML rather than repairing it.
func (c *Config) writeScalar(keys []string, tag, value string) error {
	doc, mode, err := c.editableDocument()
	if err != nil {
		return err
	}
	node := doc.Content[0]
	for _, key := range keys[:len(keys)-1] {
		if node, err = editableMapping(node, key); err != nil {
			return err
		}
	}
	last := keys[len(keys)-1]
	if i := mappingKey(node, last); i >= 0 {
		v := node.Content[i+1]
		if v.Kind != yaml.ScalarNode || v.Anchor != "" {
			return errors.New("config " + strings.Join(keys, ".") + " must be a plain value without a YAML anchor")
		}
		if v.Tag == tag && v.Value == value {
			return nil
		}
		v.Tag, v.Value, v.Style = tag, value, 0
	} else {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	return writeYAMLAtomic(c.ConfigPath(), doc, mode)
}

// editableDocument reads config.yaml for a comment-preserving edit. It is the
// general form of editableSourcesDocument.
func (c *Config) editableDocument() (*yaml.Node, os.FileMode, error) {
	data, mode, exists, err := readEditableConfig(c.ConfigPath())
	if err != nil {
		return nil, 0, err
	}
	if !exists {
		if data, err = yaml.Marshal(Default(c.dir)); err != nil {
			return nil, 0, err
		}
		mode = 0o644
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&doc); err != nil {
		return nil, 0, errors.New("config is invalid YAML; fix it before changing settings")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, 0, errors.New("config must contain exactly one YAML document")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, 0, errors.New("config root must be a YAML mapping")
	}
	if err := doc.Decode(Default(c.dir)); err != nil {
		return nil, 0, errors.New("config is invalid; fix it before changing settings")
	}
	return &doc, mode, nil
}
