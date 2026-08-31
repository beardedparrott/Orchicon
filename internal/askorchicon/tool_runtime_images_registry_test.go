package askorchicon

import (
    "encoding/json"
    "log/slog"
    "testing"

    "github.com/beardedparrott/orchicon/internal/db"
)

func TestRuntimeImageRegistryExposesNewTools(t *testing.T) {
    reg := NewToolRegistry(nil, slog.Default(), nil)
    for _, name := range []string{"create_runtime_image", "update_runtime_image", "build_runtime_image", "list_runtime_images", "get_runtime_image", "delete_runtime_image"} {
        if _, ok := reg.Get(name); !ok {
            t.Fatalf("registry missing tool %q", name)
        }
    }
    for _, name := range []string{"create_runtime_image", "update_runtime_image", "build_runtime_image", "delete_runtime_image"} {
        if !reg.IsMutating(name) {
            t.Fatalf("tool %q should be mutating", name)
        }
    }
    checkProps := func(name string, wantRequired []string, wantProps []string) {
        td, _ := reg.Get(name)
        for _, w := range wantRequired {
            found := false
            for _, r := range td.Required {
                if r == w {
                    found = true
                    break
                }
            }
            if !found {
                t.Fatalf("tool %q missing required %q (got %v)", name, w, td.Required)
            }
        }
        for _, w := range wantProps {
            if _, ok := td.Properties[w]; !ok {
                t.Fatalf("tool %q missing property %q", name, w)
            }
        }
    }
    checkProps("create_runtime_image", []string{"name", "slug"}, []string{"env", "dockerfile_override", "tag"})
    checkProps("update_runtime_image", []string{"id", "version"}, []string{"env", "dockerfile_override", "tag", "name", "slug"})
    checkProps("build_runtime_image", []string{"id", "version"}, nil)
}

func TestCreateRuntimeImageEnvValidation(t *testing.T) {
    if _, err := riValidateJSONObject(`{"PLAYWRIGHT_BROWSERS_PATH":"/ms-playwright","NODE_PATH":"/usr/local/lib/node_modules"}`, "env"); err != nil {
        t.Fatalf("valid env rejected: %v", err)
    }
    if _, err := riValidateJSONObject(`not-json`, "env"); err == nil {
        t.Fatal("invalid env should be rejected")
    }
    if _, err := riValidateJSONObject(`{"a":123}`, "env"); err == nil {
        t.Fatal("non-string env value should be rejected")
    }
    b, err := riValidateJSONObject("", "env")
    if err != nil {
        t.Fatalf("empty env: %v", err)
    }
    var m map[string]string
    if err := json.Unmarshal(b, &m); err != nil {
        t.Fatalf("empty env not JSON: %v", err)
    }
}

func TestToolRuntimeImageHelpers(t *testing.T) {
    if _, err := riValidateSlug("web-research"); err != nil {
        t.Fatalf("valid slug rejected: %v", err)
    }
    if _, err := riValidateSlug("Bad_Slug"); err == nil {
        t.Fatal("invalid slug should be rejected")
    }
    s := riTruncate("abcdef", 3)
    if s != "abc" {
        t.Fatalf("truncate got %q", s)
    }
    _ = db.Pool{}
    _ = json.RawMessage{}
}
