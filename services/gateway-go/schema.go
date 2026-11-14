package main

import (
	"fmt"
	"sort"
	"strings"
)

// Argument validation at the gateway, before the call leaves it. The service validates too — a
// service that trusts its caller is one bug away from a memory error — but a gateway that
// forwards anything it did not check cannot say "this request was never valid", and it also
// caches by arguments: garbage in, garbage cached.
//
// Supported subset of JSON Schema, deliberately small and stated: object root, properties,
// required, type (string/number/integer/boolean/array/object), enum, and
// additionalProperties=false. Anything else in a schema is passed through unchecked, and the
// function says so through its return value rather than pretending to be complete.

type schemaCheck struct {
	unknown []string // keywords present but not enforced
}

func validateArgs(schema map[string]any, args map[string]any) error {
	chk := &schemaCheck{}
	if schema == nil {
		return nil
	}
	if err := chk.object(schema, args, ""); err != nil {
		return err
	}
	return nil
}

// unchecked reports the schema keywords this validator ignored, so a caller can log them once
// instead of believing the arguments were fully verified.
func (c *schemaCheck) unchecked() []string {
	sort.Strings(c.unknown)
	return c.unknown
}

var enforcedKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "enum": true,
	"additionalProperties": true, "items": true, "description": true,
	"default": true, "title": true, "$schema": true, "$id": true,
}

func (c *schemaCheck) object(schema map[string]any, value map[string]any, path string) error {
	for k := range schema {
		if !enforcedKeywords[k] {
			c.unknown = append(c.unknown, k)
		}
	}
	if want, ok := schema["type"].(string); ok && want != "" && want != "object" {
		return fmt.Errorf("%s: schema type %q does not match an object argument", nameOf(path), want)
	}
	props, _ := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			key := asString(r)
			if key == "" {
				continue
			}
			if _, present := value[key]; !present {
				return fmt.Errorf("%smissing required argument %q", prefixOf(path), key)
			}
		}
	}
	if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
		for k := range value {
			if _, declared := props[k]; !declared {
				return fmt.Errorf("%sunknown argument %q", prefixOf(path), k)
			}
		}
	}
	for name, rawSchema := range props {
		sub, ok := rawSchema.(map[string]any)
		if !ok {
			continue
		}
		v, present := value[name]
		if !present {
			continue
		}
		if err := c.field(sub, v, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func (c *schemaCheck) field(schema map[string]any, v any, path string) error {
	if want, ok := schema["type"].(string); ok && want != "" {
		if err := c.typeOf(want, v, path); err != nil {
			return err
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		hit := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				hit = true
				break
			}
		}
		if !hit {
			return fmt.Errorf("%s: %v is not one of the allowed values", path, v)
		}
	}
	if want, ok := schema["type"].(string); ok && want == "array" {
		if items, ok := schema["items"].(map[string]any); ok {
			if list, ok := v.([]any); ok {
				for i, item := range list {
					if err := c.field(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
				}
			}
		}
	}
	if want, ok := schema["type"].(string); ok && want == "object" && v != nil {
		if sub, ok := v.(map[string]any); ok {
			return c.object(schema, sub, path)
		}
	}
	return nil
}

func (c *schemaCheck) typeOf(want string, v any, path string) error {
	switch want {
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s wants a string, got %s", path, jsonKind(v))
		}
	case "number":
		switch v.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("%s wants a number, got %s", path, jsonKind(v))
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s wants an integer, got %s", path, jsonKind(v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s wants a boolean, got %s", path, jsonKind(v))
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("%s wants an array, got %s", path, jsonKind(v))
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("%s wants an object, got %s", path, jsonKind(v))
		}
	case "null":
		if v != nil {
			return fmt.Errorf("%s wants null, got %s", path, jsonKind(v))
		}
	default:
		if !strings.HasPrefix(want, "array") {
			c.unknown = append(c.unknown, "type="+want)
		}
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func prefixOf(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

func nameOf(path string) string {
	if path == "" {
		return "arguments"
	}
	return path
}
