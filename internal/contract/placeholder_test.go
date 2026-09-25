package contract

import "testing"

// A static field name and a runtime value for the same parameter must not be
// merged: the placeholder proves the field exists, not what it holds.
func TestPlaceholderIsNotAValue(t *testing.T) {
	fields := addFieldValue(nil, "SITE_ID", "<SITE_ID>")
	fields = addFieldValue(fields, "SITE_ID", "s1")
	if len(fields[0].Values) != 1 || fields[0].Values[0] != "s1" {
		t.Fatalf("placeholder leaked into values: %+v", fields[0])
	}
	if fields = finalizeFields(fields); fields[0].Kind == "enum" {
		t.Errorf("field became an enum from a placeholder: %+v", fields[0])
	}
	// With no observed value the name is reported without inventing a type.
	only := finalizeFields(addFieldValue(nil, "REF", "<REF>"))
	if only[0].Kind != "unknown" || !only[0].Inferred {
		t.Errorf("name-only field should be unknown+inferred: %+v", only[0])
	}
}
