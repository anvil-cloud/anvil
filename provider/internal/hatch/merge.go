package hatch

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Merge deep-merges user overrides into default properties.
//
// Rules:
//   - Maps merge recursively (user keys override, default keys preserved)
//   - Scalars override
//   - Arrays replace entirely (not append)
//   - Pulumi Outputs in defaults are preserved unless explicitly overridden
func Merge(overrides map[string]interface{}, defaults pulumi.Map) pulumi.Map {
	if len(overrides) == 0 {
		return defaults
	}

	for k, v := range overrides {
		existing, hasExisting := defaults[k]

		if hasExisting {
			if merged, ok := deepMerge(existing, v); ok {
				defaults[k] = merged
				continue
			}
		}

		defaults[k] = pulumi.Any(v)
	}
	return defaults
}

// deepMerge attempts to recursively merge an override value into an existing value.
// Returns (merged, true) if both sides are map-like, or (nil, false) if they can't be merged.
func deepMerge(existing interface{}, override interface{}) (pulumi.Input, bool) {
	overrideMap, overrideIsMap := toRawMap(override)
	if !overrideIsMap {
		return nil, false
	}

	existingMap, existingIsMap := toRawMap(existing)
	if !existingIsMap {
		return nil, false
	}

	// Both sides are maps — merge recursively
	for k, v := range overrideMap {
		if existingVal, ok := existingMap[k]; ok {
			if merged, ok := deepMerge(existingVal, v); ok {
				existingMap[k] = merged
				continue
			}
		}
		existingMap[k] = pulumi.Any(v)
	}

	// Rebuild as pulumi.Map
	result := pulumi.Map{}
	for k, v := range existingMap {
		if input, ok := v.(pulumi.Input); ok {
			result[k] = input
		} else {
			result[k] = pulumi.Any(v)
		}
	}
	return result, true
}

// toRawMap normalises various map types into map[string]interface{} for uniform handling.
// Returns nil, false for non-map types (scalars, arrays, Outputs).
func toRawMap(v interface{}) (map[string]interface{}, bool) {
	switch m := v.(type) {
	case pulumi.Map:
		raw := make(map[string]interface{}, len(m))
		for k, v := range m {
			raw[k] = v
		}
		return raw, true

	case pulumi.StringMap:
		raw := make(map[string]interface{}, len(m))
		for k, v := range m {
			raw[k] = v
		}
		return raw, true

	case map[string]interface{}:
		return m, true

	default:
		return nil, false
	}
}

// MergeStringMap merges user string overrides into a default pulumi.StringMap.
// This is a convenience for tags/labels where both sides are flat string maps.
func MergeStringMap(defaults pulumi.StringMap, overrides map[string]interface{}) pulumi.StringMap {
	for k, v := range overrides {
		defaults[k] = pulumi.String(fmt.Sprint(v))
	}
	return defaults
}
