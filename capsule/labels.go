package capsule

// SameKeyword is the reserved label value that indicates "must share this label's value
// with the target entity" in capsule affinity/anti-affinity placement rules.
const SameKeyword = "same"

// LabelsMatch returns true if the candidate labels satisfy all the required labels.
// For each required label key:
//   - If the required value is a regular value, the candidate must have the same key and value.
//   - The "same" keyword is not evaluated here; use LabelsShareMatch for affinity comparison.
func LabelsMatch(candidate, required Labels) bool {
	for key, reqVal := range required {
		candVal, ok := candidate[key]
		if !ok {
			return false
		}
		if reqVal != candVal {
			return false
		}
	}
	return true
}

// LabelsShareMatch checks if two entities share the same values for the specified label keys.
// This is used for capsule affinity/anti-affinity rules where labels contain the "same" keyword.
// It extracts the keys marked as "same" from the rule labels, then checks if sourceLabels and
// targetLabels have identical values for those keys.
func LabelsShareMatch(sourceLabels, targetLabels, ruleLabels Labels) bool {
	for key, val := range ruleLabels {
		if val != SameKeyword {
			continue
		}
		sourceVal, sourceOK := sourceLabels[key]
		targetVal, targetOK := targetLabels[key]
		if !sourceOK || !targetOK {
			return false
		}
		if sourceVal != targetVal {
			return false
		}
	}
	return true
}

// LabelsMatchAnyValue returns true if the candidate has the given key with any of the allowed values.
func LabelsMatchAnyValue(candidate Labels, key string, allowedValues []string) bool {
	candVal, ok := candidate[key]
	if !ok {
		return false
	}
	for _, v := range allowedValues {
		if candVal == v {
			return true
		}
	}
	return false
}

// LabelsContains returns true if the labels contain the given key.
func LabelsContains(labels Labels, key string) bool {
	_, ok := labels[key]
	return ok
}

// LabelsMerge returns a new Labels that merges base with overrides.
// Override values take precedence.
func LabelsMerge(base, overrides Labels) Labels {
	result := make(Labels, len(base)+len(overrides))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overrides {
		result[k] = v
	}
	return result
}

// LabelsEqual returns true if two label sets are identical.
func LabelsEqual(a, b Labels) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
