package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

var (
	ErrInvalidContract  = errors.New("invalid tool contract")
	ErrSchemaValidation = errors.New("tool schema validation failed")
	ErrUnknownTool      = errors.New("unknown tool")
)

type Contract struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Capability   string          `json:"capability"`
	ReadOnly     bool            `json:"read_only"`
	Timeout      time.Duration   `json:"timeout"`
	OutputLimit  int             `json:"output_limit"`
	AllowedPaths []string        `json:"allowed_paths"`
	Network      bool            `json:"network"`
	Idempotency  string          `json:"idempotency"`
}

func (contract Contract) Validate() error {
	if contract.Name == "" ||
		contract.Version == "" ||
		len(contract.InputSchema) == 0 ||
		len(contract.OutputSchema) == 0 ||
		contract.Capability == "" ||
		contract.Timeout <= 0 ||
		contract.OutputLimit <= 0 ||
		len(contract.AllowedPaths) == 0 ||
		contract.Idempotency == "" {
		return fmt.Errorf("%w: contract %q is incomplete", ErrInvalidContract, contract.Name)
	}
	for label, schema := range map[string]json.RawMessage{
		"input": contract.InputSchema, "output": contract.OutputSchema,
	} {
		var decoded map[string]any
		if err := json.Unmarshal(schema, &decoded); err != nil {
			return fmt.Errorf("%w: %s schema: %v", ErrInvalidContract, label, err)
		}
		if decoded["type"] != "object" {
			return fmt.Errorf("%w: %s schema root must be object", ErrInvalidContract, label)
		}
		if err := validateSchemaDefinition(decoded, "$"); err != nil {
			return fmt.Errorf("%w: %s schema: %v", ErrInvalidContract, label, err)
		}
	}
	return nil
}

func (contract Contract) ValidateInput(input json.RawMessage) error {
	return validateJSONSchema(contract.InputSchema, input)
}

func (contract Contract) ValidateOutput(output json.RawMessage) error {
	return validateJSONSchema(contract.OutputSchema, output)
}

type Registry struct {
	contracts map[string]Contract
}

func NewRegistry(contracts ...Contract) (*Registry, error) {
	registry := &Registry{contracts: make(map[string]Contract, len(contracts))}
	for _, contract := range contracts {
		if err := contract.Validate(); err != nil {
			return nil, err
		}
		if _, exists := registry.contracts[contract.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate name %q", ErrInvalidContract, contract.Name)
		}
		registry.contracts[contract.Name] = cloneContract(contract)
	}
	return registry, nil
}

func NewBuiltinRegistry() (*Registry, error) {
	return NewRegistry(builtinContracts()...)
}

func (registry *Registry) Get(name string) (Contract, bool) {
	contract, ok := registry.contracts[name]
	return cloneContract(contract), ok
}

func (registry *Registry) Names() []string {
	names := make([]string, 0, len(registry.contracts))
	for name := range registry.contracts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Contracts returns a sorted deep snapshot. Registry owns its backing bytes
// and slices so a caller cannot widen a contract after Service construction.
func (registry *Registry) Contracts() []Contract {
	if registry == nil {
		return nil
	}
	names := registry.Names()
	contracts := make([]Contract, 0, len(names))
	for _, name := range names {
		contract, ok := registry.Get(name)
		if ok {
			contracts = append(contracts, contract)
		}
	}
	return contracts
}

// ContractSetsEqual compares the complete execution contract independently of
// input order. It includes both Schemas and every policy-relevant field.
func ContractSetsEqual(left, right []Contract) bool {
	if len(left) != len(right) {
		return false
	}
	rightByName := make(map[string]Contract, len(right))
	for _, contract := range right {
		if _, exists := rightByName[contract.Name]; exists {
			return false
		}
		rightByName[contract.Name] = contract
	}
	seen := make(map[string]struct{}, len(left))
	for _, contract := range left {
		if _, duplicate := seen[contract.Name]; duplicate {
			return false
		}
		seen[contract.Name] = struct{}{}
		other, ok := rightByName[contract.Name]
		if !ok || !contractsEqual(contract, other) {
			return false
		}
	}
	return true
}

func contractsEqual(left, right Contract) bool {
	return left.Name == right.Name &&
		left.Version == right.Version &&
		bytes.Equal(left.InputSchema, right.InputSchema) &&
		bytes.Equal(left.OutputSchema, right.OutputSchema) &&
		left.Capability == right.Capability &&
		left.ReadOnly == right.ReadOnly &&
		left.Timeout == right.Timeout &&
		left.OutputLimit == right.OutputLimit &&
		stringSlicesEqual(left.AllowedPaths, right.AllowedPaths) &&
		left.Network == right.Network &&
		left.Idempotency == right.Idempotency
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneContract(contract Contract) Contract {
	cloned := contract
	cloned.InputSchema = append(json.RawMessage(nil), contract.InputSchema...)
	cloned.OutputSchema = append(json.RawMessage(nil), contract.OutputSchema...)
	cloned.AllowedPaths = append([]string(nil), contract.AllowedPaths...)
	return cloned
}

func builtinContracts() []Contract {
	output := schema(`{
	  "type":"object",
	  "additionalProperties":false,
	  "required":["stdout","stderr","exitCode","timedOut","truncated"],
	  "properties":{
	    "stdout":{"type":"string"},
	    "stderr":{"type":"string"},
	    "exitCode":{"type":"integer"},
	    "timedOut":{"type":"boolean"},
	    "truncated":{"type":"boolean"}
	  }
	}`)
	return []Contract{
		{
			Name: "repo.list", Version: "v1", Capability: "repo:read",
			ReadOnly: true, Timeout: 10 * time.Second, OutputLimit: 256 << 10,
			AllowedPaths: []string{"."}, Network: false,
			Idempotency: "read-only; identical workspace bytes yield equivalent output",
			InputSchema: schema(`{
			  "type":"object","additionalProperties":false,
			  "required":["path"],
			  "properties":{"path":{"type":"string","minLength":1}}
			}`),
			OutputSchema: output,
		},
		{
			Name: "repo.read", Version: "v1", Capability: "repo:read",
			ReadOnly: true, Timeout: 10 * time.Second, OutputLimit: 256 << 10,
			AllowedPaths: []string{"."}, Network: false,
			Idempotency: "read-only; identical workspace bytes yield equivalent output",
			InputSchema: schema(`{
			  "type":"object","additionalProperties":false,
			  "required":["path"],
			  "properties":{
			    "path":{"type":"string","minLength":1},
			    "startLine":{"type":"integer","minimum":1},
			    "endLine":{"type":"integer","minimum":1}
			  }
			}`),
			OutputSchema: output,
		},
		{
			Name: "repo.search", Version: "v1", Capability: "repo:read",
			ReadOnly: true, Timeout: 15 * time.Second, OutputLimit: 512 << 10,
			AllowedPaths: []string{"."}, Network: false,
			Idempotency: "read-only; identical workspace bytes yield equivalent output",
			InputSchema: schema(`{
			  "type":"object","additionalProperties":false,
			  "required":["path","pattern"],
			  "properties":{
			    "path":{"type":"string","minLength":1},
			    "pattern":{"type":"string","minLength":1}
			  }
			}`),
			OutputSchema: output,
		},
		{
			Name: "repo.apply_patch", Version: "v1", Capability: "repo:write",
			ReadOnly: false, Timeout: 15 * time.Second, OutputLimit: 256 << 10,
			AllowedPaths: []string{"."}, Network: false,
			Idempotency: "scoped exact replacement; the complete result must exclude old so replay after success is rejected",
			InputSchema: schema(`{
			  "type":"object","additionalProperties":false,
			  "required":["path","old","new"],
			  "properties":{
			    "path":{"type":"string","minLength":1},
			    "old":{"type":"string","minLength":1},
			    "new":{"type":"string"}
			  }
			}`),
			OutputSchema: output,
		},
		{
			Name: "repo.test", Version: "v1", Capability: "repo:test",
			ReadOnly: false, Timeout: 60 * time.Second, OutputLimit: 1 << 20,
			AllowedPaths: []string{"."}, Network: false,
			Idempotency: "scoped to disposable workspace; tests may write only within that workspace",
			InputSchema: schema(`{
			  "type":"object","additionalProperties":false,
			  "required":["packages"],
			  "properties":{
			    "packages":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},
			    "run":{"type":"string"},
			    "verbose":{"type":"boolean"}
			  }
			}`),
			OutputSchema: output,
		},
	}
}

func schema(value string) json.RawMessage {
	return json.RawMessage(bytes.TrimSpace([]byte(value)))
}

func validateJSONSchema(schemaBytes, payloadBytes []byte) error {
	var definition map[string]any
	if err := json.Unmarshal(schemaBytes, &definition); err != nil {
		return fmt.Errorf("%w: invalid schema: %v", ErrSchemaValidation, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: invalid JSON: %v", ErrSchemaValidation, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: multiple JSON values", ErrSchemaValidation)
		}
		return fmt.Errorf("%w: trailing JSON: %v", ErrSchemaValidation, err)
	}
	if err := validateSchemaValue(definition, payload, "$"); err != nil {
		return fmt.Errorf("%w: %v", ErrSchemaValidation, err)
	}
	return nil
}

func validateSchemaDefinition(definition map[string]any, location string) error {
	expected, ok := definition["type"].(string)
	if !ok {
		return fmt.Errorf("%s.type must be a supported string", location)
	}
	allowedKeywords := map[string]bool{"type": true}
	switch expected {
	case "object":
		allowedKeywords["additionalProperties"] = true
		allowedKeywords["required"] = true
		allowedKeywords["properties"] = true
		if additional, exists := definition["additionalProperties"]; exists {
			if _, ok := additional.(bool); !ok {
				return fmt.Errorf("%s.additionalProperties must be boolean", location)
			}
		}
		properties := map[string]any{}
		if rawProperties, exists := definition["properties"]; exists {
			var propertiesOK bool
			properties, propertiesOK = rawProperties.(map[string]any)
			if !propertiesOK {
				return fmt.Errorf("%s.properties must be object", location)
			}
		}
		if rawRequired, exists := definition["required"]; exists {
			required, ok := rawRequired.([]any)
			if !ok {
				return fmt.Errorf("%s.required must be an array of property names", location)
			}
			seen := make(map[string]bool, len(required))
			for _, raw := range required {
				name, ok := raw.(string)
				if !ok || name == "" || seen[name] {
					return fmt.Errorf("%s.required contains an invalid property name", location)
				}
				if _, exists := properties[name]; !exists {
					return fmt.Errorf("%s.required references undeclared property %q", location, name)
				}
				seen[name] = true
			}
		}
		for name, raw := range properties {
			child, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties.%s must be a schema object", location, name)
			}
			if err := validateSchemaDefinition(child, location+".properties."+name); err != nil {
				return err
			}
		}
	case "array":
		allowedKeywords["minItems"] = true
		allowedKeywords["items"] = true
		if minimum, exists := definition["minItems"]; exists && !nonNegativeSchemaInteger(minimum) {
			return fmt.Errorf("%s.minItems must be a non-negative integer", location)
		}
		rawItems, exists := definition["items"]
		if !exists {
			return fmt.Errorf("%s.items is required", location)
		}
		items, ok := rawItems.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.items must be a schema object", location)
		}
		if err := validateSchemaDefinition(items, location+".items"); err != nil {
			return err
		}
	case "string":
		allowedKeywords["minLength"] = true
		if minimum, exists := definition["minLength"]; exists && !nonNegativeSchemaInteger(minimum) {
			return fmt.Errorf("%s.minLength must be a non-negative integer", location)
		}
	case "integer":
		allowedKeywords["minimum"] = true
		if minimum, exists := definition["minimum"]; exists && !schemaInteger(minimum) {
			return fmt.Errorf("%s.minimum must be an integer", location)
		}
	case "boolean":
	default:
		return fmt.Errorf("%s has unsupported schema type %q", location, expected)
	}
	for keyword := range definition {
		if !allowedKeywords[keyword] {
			return fmt.Errorf("%s uses unsupported schema keyword %q", location, keyword)
		}
	}
	return nil
}

func schemaInteger(value any) bool {
	number, ok := value.(float64)
	return ok && number == float64(int64(number))
}

func nonNegativeSchemaInteger(value any) bool {
	number, ok := value.(float64)
	return ok && number >= 0 && number == float64(int64(number))
}

func validateSchemaValue(definition map[string]any, value any, location string) error {
	expected, _ := definition["type"].(string)
	switch expected {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", location)
		}
		properties, _ := definition["properties"].(map[string]any)
		required, _ := definition["required"].([]any)
		for _, raw := range required {
			name, _ := raw.(string)
			if _, exists := object[name]; !exists {
				return fmt.Errorf("%s.%s is required", location, name)
			}
		}
		additional, hasAdditional := definition["additionalProperties"].(bool)
		for name, child := range object {
			rawChild, exists := properties[name]
			if !exists {
				if hasAdditional && !additional {
					return fmt.Errorf("%s.%s is not allowed", location, name)
				}
				continue
			}
			childDefinition, ok := rawChild.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s has invalid schema", location, name)
			}
			if err := validateSchemaValue(childDefinition, child, location+"."+name); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", location)
		}
		if minimum, ok := numericInt(definition["minItems"]); ok && len(array) < minimum {
			return fmt.Errorf("%s must contain at least %d items", location, minimum)
		}
		if rawItems, ok := definition["items"].(map[string]any); ok {
			for index, item := range array {
				if err := validateSchemaValue(rawItems, item, fmt.Sprintf("%s[%d]", location, index)); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be string", location)
		}
		if minimum, ok := numericInt(definition["minLength"]); ok && len(text) < minimum {
			return fmt.Errorf("%s must have length >= %d", location, minimum)
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be integer", location)
		}
		if _, err := number.Int64(); err != nil {
			return fmt.Errorf("%s must be integer", location)
		}
		integer, _ := number.Int64()
		if minimum, ok := numericInt(definition["minimum"]); ok && integer < int64(minimum) {
			return fmt.Errorf("%s must be >= %d", location, minimum)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", location)
		}
	default:
		return fmt.Errorf("%s has unsupported schema type %q", location, expected)
	}
	return nil
}

func numericInt(value any) (int, bool) {
	number, ok := value.(float64)
	return int(number), ok
}
