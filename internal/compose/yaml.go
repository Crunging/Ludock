package compose

import (
	"errors"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Crunging/Ludock/internal/apperr"
)

// aliasLimit bounds alias expansion: enough for a shared service template,
// while nested expansion stays small.
const aliasLimit = 20

var (
	errInvalidYAML = apperr.New("INVALID_COMPOSE", 400, "Compose YAML could not be parsed safely")
	errTags        = apperr.New("UNSUPPORTED_COMPOSE", 400, "Compose YAML tags such as !reset and !override are outside the supported update configuration")
)

// Explicit tags that keep their meaning in JSON. Others, including
// !!binary, !!set, and Compose's !reset and !override, are rejected.
var jsonTags = map[string]bool{"!!str": true, "!!int": true, "!!float": true, "!!bool": true, "!!null": true,
	"!!map": true, "!!seq": true, "!!merge": true}

type decoder struct{ aliases int }

// ParseFile reads one Compose file as Compose does: merge keys apply,
// duplicate keys fail, and alias expansion stays bounded.
func ParseFile(text []byte) (map[string]any, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(text, &document); err != nil {
		return nil, errInvalidYAML
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil, errInvalidYAML
	}
	d := &decoder{}
	value, err := d.value(document.Content[0], 0)
	if err != nil {
		return nil, err
	}
	model, ok := value.(map[string]any)
	if !ok {
		return nil, apperr.New("UNSUPPORTED_COMPOSE", 400, "Unsupported Compose structure")
	}
	return model, nil
}

func (d *decoder) value(node *yaml.Node, depth int) (any, error) {
	if depth > 100 {
		return nil, errInvalidYAML
	}
	if node.Tag != "" && !jsonTags[node.Tag] && node.Kind != yaml.AliasNode {
		return nil, errTags
	}
	switch node.Kind {
	case yaml.AliasNode:
		d.aliases++
		if d.aliases > aliasLimit {
			return nil, errInvalidYAML
		}
		return d.value(node.Alias, depth+1)
	case yaml.ScalarNode:
		return scalar(node)
	case yaml.SequenceNode:
		result := []any{}
		for _, child := range node.Content {
			value, err := d.value(child, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.MappingNode:
		return d.mapping(node, depth)
	}
	return nil, errInvalidYAML
}

func (d *decoder) mapping(node *yaml.Node, depth int) (map[string]any, error) {
	result := map[string]any{}
	own := map[string]bool{}
	var merged []map[string]any
	for index := 0; index+1 < len(node.Content); index += 2 {
		keyNode, valueNode := node.Content[index], node.Content[index+1]
		if keyNode.Tag == "!!merge" {
			sources, err := d.mergeSources(valueNode, depth)
			if err != nil {
				return nil, err
			}
			merged = append(merged, sources...)
			continue
		}
		if keyNode.Kind != yaml.ScalarNode || (keyNode.Tag != "" && !jsonTags[keyNode.Tag]) {
			return nil, errInvalidYAML
		}
		key := keyNode.Value
		if own[key] {
			return nil, errInvalidYAML
		}
		own[key] = true
		value, err := d.value(valueNode, depth+1)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	// Earlier merge sources win over later ones; own keys win over all.
	for _, source := range merged {
		for key, value := range source {
			if _, exists := result[key]; !exists {
				result[key] = value
			}
		}
	}
	return result, nil
}

func (d *decoder) mergeSources(node *yaml.Node, depth int) ([]map[string]any, error) {
	value, err := d.value(node, depth+1)
	if err != nil {
		return nil, err
	}
	switch typed := value.(type) {
	case map[string]any:
		return []map[string]any{typed}, nil
	case []any:
		var result []map[string]any
		for _, item := range typed {
			source, ok := item.(map[string]any)
			if !ok {
				return nil, errInvalidYAML
			}
			result = append(result, source)
		}
		return result, nil
	}
	return nil, errInvalidYAML
}

func scalar(node *yaml.Node) (any, error) {
	switch node.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var value bool
		if err := node.Decode(&value); err != nil {
			return nil, errInvalidYAML
		}
		return value, nil
	case "!!int":
		var value int64
		if err := node.Decode(&value); err != nil {
			if parsed, parseErr := strconv.ParseFloat(strings.ReplaceAll(node.Value, "_", ""), 64); parseErr == nil {
				return parsed, nil
			}
			return nil, errInvalidYAML
		}
		return value, nil
	case "!!float":
		var value float64
		if err := node.Decode(&value); err != nil {
			return nil, errInvalidYAML
		}
		return value, nil
	case "!!str", "":
		return node.Value, nil
	}
	return nil, errors.New("unsupported scalar")
}
