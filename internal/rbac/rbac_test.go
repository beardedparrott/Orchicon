package rbac

import "testing"

func TestEntitlementForReadAndWrite(t *testing.T) {
	tests := []struct {
		procedure string
		want      Entitlement
	}{
		{"/orchicon.api.v1.ProjectService/CreateProject", "project:write"},
		{"/orchicon.api.v1.ProjectService/GetProject", "project:read"},
		{"/orchicon.api.v1.ProjectService/ListProjects", "project:read"},
		{"/orchicon.api.v1.WorkerService/PublishWorkerVersion", "worker:publish"},
		{"/orchicon.api.v1.PolicyService/SupersedePolicy", "policy:supersede"},
		{"/orchicon.api.v1.WebhookService/ListSubscriptions", "webhook:read"},
		{"/orchicon.api.v1.AuthService/CreateApiKey", "auth:write"},
	}
	for _, tt := range tests {
		got := EntitlementFor(tt.procedure)
		if got != tt.want {
			t.Errorf("EntitlementFor(%q) = %q, want %q", tt.procedure, got, tt.want)
		}
	}
}

func TestHas(t *testing.T) {
	granted := []string{"project:read", "project:write", "worker:*"}
	if !Has(granted, "project:write") {
		t.Fatal("missing project:write")
	}
	if !Has(granted, "worker:publish") {
		t.Fatal("worker:* should match worker:publish")
	}
	if Has(granted, "policy:supersede") {
		t.Fatal("should not have policy:supersede")
	}
	if !Has([]string{"*"}, "anything:whatever") {
		t.Fatal("wildcard should pass")
	}
}
