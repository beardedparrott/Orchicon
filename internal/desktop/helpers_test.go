package desktop

import (
	"context"
	"os"
	"time"
)

// contextWithTimeout is the Close() timeout the tests use.
func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// testInfo is the payload the injection tests assert over.
func testInfo() Info {
	return BuildInfo("http://127.0.0.1:8080", "v-app", "v-plane")
}

// readSource reads a file from this package's directory.
func readSource(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
