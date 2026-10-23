package instancectl

// stub_test.go — a planeOps that answers without touching a plane.
//
// The contract depends on a four-method INTERFACE (planeOps), not on planebind's concrete Service, so every
// outcome a real plane produces only sometimes — a stop that leaves an unidentifiable process alone, a
// restart that half-succeeded — is drivable here. planebind's own seams are unexported by design (a caller
// must not be able to fake a result in production), which is right, and which is why the seam belongs on
// this side of the boundary.

import (
	"errors"

	"github.com/beardedparrott/orchicon/internal/planebind"
)

// stubPlane answers with fixed DTOs.
type stubPlane struct {
	status planebind.StatusDTO
	action planebind.ActionDTO
	err    error
	calls  []string
}

func (s *stubPlane) Status(name string) (planebind.StatusDTO, error) {
	s.calls = append(s.calls, "status")
	dto := s.status
	if dto.Instance == "" {
		dto.Instance = name
	}
	return dto, s.err
}

func (s *stubPlane) Start(string) (planebind.ActionDTO, error) {
	s.calls = append(s.calls, "start")
	return s.action, s.err
}
func (s *stubPlane) Stop(string) (planebind.ActionDTO, error) {
	s.calls = append(s.calls, "stop")
	return s.action, s.err
}
func (s *stubPlane) Restart(string) (planebind.ActionDTO, error) {
	s.calls = append(s.calls, "restart")
	return s.action, s.err
}

var _ planeOps = (*stubPlane)(nil)

// stubErr is a convenient failure for the error paths.
var stubErr = errors.New("stub failure")
