package db

import "testing"

func TestCanonicalRuntimeImageRewritesOnlyRetiredTags(t *testing.T) {
	cases := map[string]string{
		"orchicon-runtime:orchicon-dev":           "orchicon-runtime:dev",
		"localhost/orchicon-runtime:orchicon-dev": "localhost/orchicon-runtime:dev",
		"orchicon-runtime:dev":                    "orchicon-runtime:dev",
		"orchicon-runtime:gui":                    "orchicon-runtime:gui",
		"my/custom:orchicon-dev":                  "my/custom:orchicon-dev", // someone else's image is never rewritten
		"":                                        "",
	}
	for in, want := range cases {
		if got := CanonicalRuntimeImage(in); got != want {
			t.Errorf("CanonicalRuntimeImage(%q) = %q, want %q", in, got, want)
		}
	}
}
