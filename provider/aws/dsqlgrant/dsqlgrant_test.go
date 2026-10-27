package dsqlgrant

import (
	"strings"
	"testing"
)

func TestValidateRoles(t *testing.T) {
	declared := map[string]string{"orders_writer": "shop", "invoices_rw": "billing"}

	if err := validateRoles([]string{"orders_writer", "invoices_rw"}, nil, declared); err != nil {
		t.Errorf("declared roles rejected: %v", err)
	}
	if err := validateRoles([]string{"orders_writer"}, map[string]string{"orders_writer": "shop"}, declared); err != nil {
		t.Errorf("matching schema rejected: %v", err)
	}

	err := validateRoles([]string{"ghost"}, nil, declared)
	if err == nil || !strings.Contains(err.Error(), `role "ghost" isn't declared`) ||
		!strings.Contains(err.Error(), "invoices_rw (billing), orders_writer (shop)") {
		t.Errorf("undeclared role: %v, want an error listing the declared roles", err)
	}

	err = validateRoles([]string{"orders_writer"}, map[string]string{"orders_writer": "billing"}, declared)
	if err == nil || !strings.Contains(err.Error(), `declared in schema "shop", not "billing"`) {
		t.Errorf("schema mismatch: %v", err)
	}

	err = validateRoles([]string{"orders_writer"}, nil, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "none — declare roles") {
		t.Errorf("no declared roles: %v", err)
	}
}

func TestItemVersion(t *testing.T) {
	a := map[string]map[string]string{"roleName": {"S": "r"}, "sk": {"S": "arn:aws:iam::1:role/x"}}
	b := map[string]map[string]string{"sk": {"S": "arn:aws:iam::1:role/x"}, "roleName": {"S": "r"}}
	if ItemVersion(a) != ItemVersion(b) {
		t.Error("version depends on map order")
	}
	if len(ItemVersion(a)) != 16 {
		t.Errorf("version %q, want 16 hex chars", ItemVersion(a))
	}
	b["sk"] = map[string]string{"S": "arn:aws:iam::1:role/y"}
	if ItemVersion(a) == ItemVersion(b) {
		t.Error("different contents gave the same version")
	}
}
