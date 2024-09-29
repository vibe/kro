// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package parser

import (
	"fmt"
	"strings"

	"k8s.io/kube-openapi/pkg/validation/spec"
)

// ExpressionField represents a field that contains CEL expressions
// and the expected type of the field. The field may contain multiple
// expressions.
type ExpressionField struct {
	// Path is the path of the field in the resource (JSONPath-like)
	// example: spec.template.spec.containers[0].env[0].value
	// Since the object's we're dealing with are mainly made up of maps,
	// arrays and native types, we can use a string to represent the path.
	Path string
	// Expressions is a list of CEL expressions in the field.
	Expressions []string
	// ExpectedType is the expected type of the field.
	ExpectedType string
	// ExpectedSchema is the expected schema of the field if it is a complex type.
	// This is only set if the field is a OneShotCEL expression, and the schema
	// is expected to be a complex type (object or array).
	ExpectedSchema *spec.Schema
	// OneShotCEL is true if the field contains a single CEL expression
	// that is not part of a larger string. example: "${foo}" vs "hello-${foo}"
	OneShotCEL bool
}

// ParseResource extracts CEL expressions from a resource based on
// the schema. The resource is expected to be a map[string]interface{}.
//
// Note that this function will also validate the resource against the schema
// and return an error if the resource does not match the schema. When CEL
// expressions are found, they are extracted and returned with the expected
// type of the field (inferred from the schema).
func ParseResource(resource map[string]interface{}, resourceSchema *spec.Schema) ([]ExpressionField, error) {
	return parseResource(resource, resourceSchema, "")
}

// parseResource is a helper function that recursively extracts CEL expressions
// from a resource. It uses a depthh first search to traverse the resource and
// extract expressions from string fields
func parseResource(resource interface{}, schema *spec.Schema, path string) ([]ExpressionField, error) {
	var expressionsFields []ExpressionField
	if schema == nil {
		return expressionsFields, fmt.Errorf("schema is nil for path %s", path)
	}

	if len(schema.Type) != 1 {
		if len(schema.OneOf) > 0 {
			// TODO: Handle oneOf
			schema.Type = []string{schema.OneOf[0].Type[0]}
		} else {
			return nil, fmt.Errorf("found schema type that is not a single type: %v", schema.Type)
		}
	}

	// Determine the expected type
	expectedType := schema.Type[0]
	if expectedType == "" && schema.AdditionalProperties != nil && schema.AdditionalProperties.Allows {
		expectedType = "any"
	}

	switch field := resource.(type) {
	case map[string]interface{}:
		if expectedType != "object" && (schema.AdditionalProperties == nil || !schema.AdditionalProperties.Allows) {
			return nil, fmt.Errorf("expected object type or AdditionalProperties allowed for path %s, got %v", path, field)
		}

		for field, value := range field {
			fieldSchema, err := getFieldSchema(schema, field)
			if err != nil {
				return nil, fmt.Errorf("error getting field schema for path %s: %v", path+"."+field, err)
			}
			fieldPath := path + "." + field
			fieldExpressions, err := parseResource(value, fieldSchema, fieldPath)
			if err != nil {
				return nil, err
			}
			expressionsFields = append(expressionsFields, fieldExpressions...)
		}
	case []interface{}:
		if expectedType != "array" {
			return nil, fmt.Errorf("expected array type for path %s, got %v", path, field)
		}
		var itemSchema *spec.Schema

		if schema.Items != nil && schema.Items.Schema != nil {
			// case 1 - schema defined in Items.Schema
			itemSchema = schema.Items.Schema
		} else if schema.Items != nil && schema.Items.Schema != nil && len(schema.Items.Schema.Properties) > 0 {
			// Case 2: schema defined in Properties
			itemSchema = &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type:       []string{"object"},
					Properties: schema.Properties,
				},
			}
		} else {
			// If neither Items.Schema nor Properties are defined, we can't proceed
			return nil, fmt.Errorf("invalid array schema for path %s: neither Items.Schema nor Properties are defined", path)
		}
		for i, item := range field {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			itemExpressions, err := parseResource(item, itemSchema, itemPath)
			if err != nil {
				return nil, err
			}
			expressionsFields = append(expressionsFields, itemExpressions...)
		}
	case string:
		ok, err := isOneShotExpression(field)
		if err != nil {
			return nil, err
		}
		if ok {
			expressionsFields = append(expressionsFields, ExpressionField{
				Expressions:    []string{strings.Trim(field, "${}")},
				ExpectedType:   expectedType,
				ExpectedSchema: schema,
				Path:           path,
				OneShotCEL:     true,
			})
		} else {
			if expectedType != "string" && expectedType != "any" {
				return nil, fmt.Errorf("expected string type or AdditionalProperties allowed for path %s, got %v", path, field)
			}
			expressions, err := extractExpressions(field)
			if err != nil {
				return nil, err
			}
			if len(expressions) > 0 {
				expressionsFields = append(expressionsFields, ExpressionField{
					Expressions:  expressions,
					ExpectedType: expectedType,
					Path:         path,
				})
			}
		}
	default:
		if expectedType == "any" {
			return expressionsFields, nil
		}
		switch expectedType {
		case "number":
			if _, ok := field.(float64); !ok {
				return nil, fmt.Errorf("expected number type for path %s, got %T", path, field)
			}
		case "integer":
			_, isInt := field.(int)
			_, isInt64 := field.(int64)
			_, isInt32 := field.(int32)

			if !isInt && !isInt64 && !isInt32 {
				return nil, fmt.Errorf("expected integer type for path %s, got %T", path, field)
			}
		case "boolean":
			if _, ok := field.(bool); !ok {
				return nil, fmt.Errorf("expected boolean type for path %s, got %T", path, field)
			}
		default:
			return nil, fmt.Errorf("unexpected type for path %s: %T", path, field)
		}
	}

	return expressionsFields, nil
}

func getFieldSchema(schema *spec.Schema, field string) (*spec.Schema, error) {
	if schema.Properties != nil {
		if fieldSchema, ok := schema.Properties[field]; ok {
			return &fieldSchema, nil
		}
	}

	if schema.AdditionalProperties != nil {
		if schema.AdditionalProperties.Schema != nil {
			// If AdditionalProperties is defined with a schema, use that for all fields
			return schema.AdditionalProperties.Schema, nil
		} else if schema.AdditionalProperties.Allows {
			// Need to handle this properly
			return &spec.Schema{}, nil
		}
	}

	return nil, fmt.Errorf("schema not found for field %s", field)
}