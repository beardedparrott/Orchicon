package instancectl

// service.go — the package functions behind a TYPE, so a GUI's transport can depend on an interface.
//
// The package-level functions are the implementation (List, Status, Install, …). This type exists only so
// a binding layer can hold something and a test can substitute it: Go interfaces are satisfied by types,
// and the desktop's control surface takes an interface by design so it never links the real thing in a
// test. Nothing here adds behaviour — every method delegates.

import "io"

// Service is the value a GUI binds. Its zero value is usable.
type Service struct {
	// Out receives an operation's progress text. nil discards it, which is what a GUI wants: it reads
	// the setup record instead.
	Out io.Writer
}

// NewService returns a Service writing progress to out (nil is fine).
func NewService(out io.Writer) *Service { return &Service{Out: out} }

// List returns every instance on this machine.
func (s *Service) List() ([]Instance, error) { return List() }

// Status returns one instance.
func (s *Service) Status(name string) (Instance, error) { return Status(name) }

// Install spawns the CLI's install for name and returns immediately; watch SetupProgress.
func (s *Service) Install(name string, opts Options) (Result, error) {
	opts.Out = s.writer(opts.Out)
	return Install(name, opts)
}

// Uninstall removes name (or every instance when name is "") — keep-data unless opts.Purge.
func (s *Service) Uninstall(name string, opts Options) (Result, error) {
	opts.Out = s.writer(opts.Out)
	return Uninstall(name, opts)
}

// Start brings name up.
func (s *Service) Start(name string, opts Options) (Result, error) {
	opts.Out = s.writer(opts.Out)
	return Start(name, opts)
}

// Stop takes name down.
func (s *Service) Stop(name string, opts Options) (Result, error) {
	opts.Out = s.writer(opts.Out)
	return Stop(name, opts)
}

// Restart stops then starts name.
func (s *Service) Restart(name string, opts Options) (Result, error) {
	opts.Out = s.writer(opts.Out)
	return Restart(name, opts)
}

// SetupProgress returns name's setup record, or nil when none exists.
func (s *Service) SetupProgress(name string) (*SetupProgress, error) { return ReadSetupProgress(name) }

// writer prefers a caller-supplied sink over the Service's own, so a per-call capture is possible.
func (s *Service) writer(caller io.Writer) io.Writer {
	if caller != nil {
		return caller
	}
	return s.Out
}
