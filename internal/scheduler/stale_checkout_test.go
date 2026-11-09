package scheduler

// stale_checkout_test.go — the main checkout must be refreshable when it LAGS its own HEAD.

import "testing"

func TestStaleCheckoutOnlyAcceptsDeletedTrackedFiles(t *testing.T) {
	// Every line is "deleted in the working tree, present at HEAD" — the reported shape.
	status := " D index.html\n D package.json\n D src/App.jsx\n D tests/compare.test.js\n"
	stale, why := staleCheckoutOnly(status)
	if !stale {
		t.Fatalf("a tree of missing tracked files was not recognised as stale (%q) — the checkout can never be "+
			"refreshed, which is how it stayed empty while its ref was populated", why)
	}
	if why == "" {
		t.Error("the stale shape must carry a reason for the log line")
	}
}

func TestStaleCheckoutOnlyRefusesRealWork(t *testing.T) {
	cases := map[string]string{
		"untracked file":   "?? notes.txt\n",
		"modified file":    " M index.html\n",
		"staged change":    "A  new-file.txt\n",
		"staged + deleted": "M  a.txt\n D b.txt\n",
		"mixed":            " D gone.txt\n?? keep-me.txt\n",
	}
	for name, status := range cases {
		if stale, _ := staleCheckoutOnly(status); stale {
			t.Errorf("%s was treated as merely-stale — refreshing would destroy work that exists nowhere else: %q",
				name, status)
		}
	}
}

func TestStaleCheckoutOnlyIgnoresACleanTree(t *testing.T) {
	for _, status := range []string{"", "\n"} {
		if stale, _ := staleCheckoutOnly(status); stale {
			t.Errorf("a CLEAN tree (%q) was called stale; clean is handled by the caller", status)
		}
	}
}
