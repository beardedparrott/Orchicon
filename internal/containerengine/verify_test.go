package containerengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestVerifyRunsATrivialContainer is AC4: "installed" is not "working", so the argv is the
// proof — a real container run, bounded, with the image spelled out.
func TestVerifyRunsATrivialContainer(t *testing.T) {
	var got []string
	p := testProbe("linux", nil, nil, func(bin string, args []string) (string, error) {
		got = append([]string{bin}, args...)
		return "", nil
	})
	if err := Verify(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, "", 0); err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := []string{fakePodman, "run", "--rm", DefaultVerifyImage, "/bin/true"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}

// TestVerifySurfacesTheEnginesReason: "exit status 125" alone tells nobody anything.
func TestVerifySurfacesTheEnginesReason(t *testing.T) {
	p := testProbe("linux", nil, nil, func(string, []string) (string, error) {
		return "Error: crun: write to sync fd: No space left on device\nmore detail", errors.New("exit status 126")
	})
	err := Verify(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, "alpine", 0)
	if err == nil {
		t.Fatal("a failing container run must fail verification")
	}
	if !strings.Contains(err.Error(), "No space left on device") {
		t.Fatalf("the engine's own reason must be carried: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot run a container") {
		t.Fatalf("message: %v", err)
	}
}

// TestVerifyTimesOut: a hung engine must not hang the install.
func TestVerifyTimesOut(t *testing.T) {
	p := testProbe("linux", nil, nil, func(string, []string) (string, error) {
		return "", nil
	})
	blocking := p
	blocking.Run = func(ctx context.Context, bin string, args ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	err := Verify(context.Background(), blocking, Engine{Name: EnginePodman, Bin: fakePodman}, "alpine", 30*time.Millisecond)
	if err == nil {
		t.Fatal("a timed-out verify must fail")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("message: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("verify took %s — the timeout did not bound it", elapsed)
	}
}

// TestVerifyWithoutAnEngine: nothing to verify is a failure, not a silent pass.
func TestVerifyWithoutAnEngine(t *testing.T) {
	p := testProbe("linux", nil, nil, func(string, []string) (string, error) { return "", nil })
	if err := Verify(context.Background(), p, Engine{Name: EngineNone}, "", 0); err == nil {
		t.Fatal("verifying no engine must fail")
	}
	if err := Verify(context.Background(), p, Engine{Name: EnginePodman}, "", 0); err == nil {
		t.Fatal("verifying an empty binary must fail")
	}
}
