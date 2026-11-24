package runtime

import "os"

// readFileForTest reads a file from this package's directory, for source invariants over code that
// cannot be exercised without a live container engine.
func readFileForTest(name string) ([]byte, error) { return os.ReadFile(name) }
