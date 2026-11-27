package antigravity

// workershim.go — the three thin re-exports of the SHARED rule set that the mapper and the policy need,
// written once so neither file imports internal/workerrestrict and internal/adapter directly for a
// one-line call.
//
// IT IS NOT A SECOND POLICY. Every function here forwards to the single implementation:
// workerrestrict.OutsideProject, workerrestrict.MatchCommand and adapter.ParseModelRef. Its only job is
// to keep the mapper's imports reading as "callbacks + transcript" rather than "and also the sandbox
// rules", and to give the tests one place to see that the shared rule set is what is consulted.

import (
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// workerOutsideProject reports whether a path target escapes the project boundary, and the rule that
// refuses it. It is workerrestrict.OutsideProject, whose carve-outs (/tmp/orchicon and the .orchicon run
// metadata) are the ones BOTH other adapters already honour.
func workerOutsideProject(target, projectDir string) (allowed bool, rule string) {
	return workerrestrict.OutsideProject(target, projectDir)
}

// workerMatchCommand reports the first worker deny rule a shell command matches.
func workerMatchCommand(cmd string) (rule string, denied bool) {
	return workerrestrict.MatchCommand(cmd)
}

// parseModelRefLoose extracts the model segment from a model_ref. A nil registry is deliberate: this
// layer needs the model STRING, not a provider decision, and a structurally invalid ref must not fail a
// session that is otherwise fine (the dispatch path already validated it).
func parseModelRefLoose(ref string) (string, error) {
	mr, err := adapter.ParseModelRef(ref, nil)
	if err != nil {
		return "", err
	}
	return mr.Model, nil
}

// workerCarveOuts returns the two directory carve-outs (/tmp/orchicon and the .orchicon run metadata)
// as absolute directory paths — the SAME list internal/claude passes to claude as --add-dir.
func workerCarveOuts(projectDir string) []string {
	return workerrestrict.CarveOutDirs(projectDir)
}
