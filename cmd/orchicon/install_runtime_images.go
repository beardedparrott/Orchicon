package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	assets "github.com/beardedparrott/orchicon"
)

// runtimeImageChoice is one runtime image the install may fetch, with the size
// the prompt shows so the choice is informed.
type runtimeImageChoice struct {
	Variant string
	Ref     string
	// Published says whether Ref names something in a REGISTRY (pull) or a LOCAL tag this host builds from
	// its embedded template. The fetch below routes on it, so the two kinds cannot be confused.
	Published bool
	// BuildContext is the directory `docker build` runs from for an unpublished variant. The variants need
	// none of their own files (the Dockerfile arrives on stdin), but docker still requires a context.
	BuildContext string
	Label        string
	// SizeBytes is the approximate download size, used ONLY to render the prompt.
	SizeBytes int64
}

// ONLY THE BASE IS A REGISTRY REF, AND THAT IS THE POINT NOW RATHER THAN AN ACCIDENT.
func installRuntimeImageChoices(imageTag string) []runtimeImageChoice {
	return []runtimeImageChoice{
		{Variant: "base", Ref: "ghcr.io/beardedparrott/orchicon-runtime:" + imageTag, Published: true,
			Label: "runtime base", SizeBytes: 1_000_000_000},
		{Variant: "gui", Ref: localVariantTag(localRepoName(runtimeImageRepo), "gui"), Published: false,
			BuildContext: runtimeImageBuildContext("gui"),
			Label:        "runtime GUI (+150 MB, built locally)", SizeBytes: 150_000_000},
		{Variant: "dev", Ref: localVariantTag(localRepoName(runtimeImageRepo), "dev"), Published: false,
			BuildContext: runtimeImageBuildContext("dev"),
			Label:        "runtime dev (+1.5-2.5 GB, built locally)", SizeBytes: 2_000_000_000},
	}
}

// parseRuntimeImageSelection parses the operator's answer into zero-based
// indexes into the choice list. Pure, so the contract is testable without a
// terminal:
//
//	""            → no selection (the default: declining everything is valid)
//	"none"        → no selection
//	"all"         → every choice
//	"1,3" / "1 3" → those choices (1-based, as printed)
//	"0" / "9"     → error (out of range)
//
// A selection that includes any variant but not the base AUTO-INCLUDES the
// base: every variant is built FROM it, and a variant without its base is the
// failure this prevents.
func parseRuntimeImageSelection(input string, n int) ([]int, error) {
	s := strings.ToLower(strings.TrimSpace(input))
	if s == "" || s == "none" || s == "no" || s == "n" {
		return nil, nil
	}
	if s == "all" || s == "a" || s == "yes" || s == "y" {
		all := make([]int, 0, n)
		for i := 0; i < n; i++ {
			all = append(all, i)
		}
		return all, nil
	}
	seen := make(map[int]bool, n)
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		v, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number — enter the list numbers (e.g. 1,3), \"all\", or press Enter for none", f)
		}
		if v < 1 || v > n {
			return nil, fmt.Errorf("%d is out of range (choose 1-%d)", v, n)
		}
		seen[v-1] = true
	}
	out := make([]int, 0, len(seen)+1)
	for i := range seen {
		out = append(out, i)
	}
	sort.Ints(out)
	// The base is implicit: every variant derives FROM it.
	if len(out) > 0 && out[0] != 0 {
		out = append([]int{0}, out...)
	}
	return out, nil
}

// humanSize renders a byte count for the prompt.
func humanSize(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("~%.1f GB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("~%d MB", n/1_000_000)
	}
	return fmt.Sprintf("~%d KB", n/1_000)
}

// resolveInstallRuntimeImages asks which runtime images to download now and returns the chosen
// ones.
const runtimeImagesAnsweredFile = "runtime-images-answered"

// runtimeImagesEnv is the EXPLICIT, non-interactive answer to "which runtime images?".
//
// WHY IT EXISTS. Everything below assumes a HUMAN: the prompt needs a terminal, and
// ORCHICON_ASSUME_YES is a blanket "yes" that means ALL. That left a gap for a
// GRAPHICAL installer — the Windows MSI — which asks the operator for a multi-select
// in its own UI and then runs `orchicon install` with no console. Its answer had
// nowhere to go: the non-interactive arm returns "none" before the recorded-answer
// file is even read, so a wizard's choice of "base + dev" silently installed nothing.
//
// It takes the SAME grammar the prompt accepts (parseRuntimeImageSelection), so the
// command line, the prompt and an installer cannot disagree about what "1,3" means:
//
//	unset/empty → no answer (fall through to the rest of the resolution)
//	none        → answered: fetch nothing
//	all         → answered: fetch everything
//	1,3 / "1 3" → answered: those choices (1-based, as printed)
//	garbage     → an ERROR, not a silent "none": an installer that asked a question
//	              and then fetched nothing is worse than one that fails and says why.
const runtimeImagesEnv = "ORCHICON_INSTALL_RUNTIME_IMAGES"

// explicitRuntimeImages reads runtimeImagesEnv. The second return says whether an
// answer was given at all, so a caller can tell "answered nothing" from "unset".
func explicitRuntimeImages(promptOut io.Writer, imageTag string) ([]runtimeImageChoice, bool, error) {
	raw := strings.TrimSpace(os.Getenv(runtimeImagesEnv))
	if raw == "" {
		return nil, false, nil
	}
	choices := installRuntimeImageChoices(imageTag)
	idx, err := parseRuntimeImageSelection(raw, len(choices))
	if err != nil {
		return nil, true, fmt.Errorf("%s=%q: %w", runtimeImagesEnv, raw, err)
	}
	if len(idx) == 0 {
		fmt.Fprintf(promptOut, "  runtime images: none selected (answered by %s)\n", runtimeImagesEnv)
		return nil, true, nil
	}
	out := make([]runtimeImageChoice, 0, len(idx))
	var total int64
	for _, i := range idx {
		out = append(out, choices[i])
		total += choices[i].SizeBytes
	}
	fmt.Fprintf(promptOut, "  runtime images: %d selected (%s) — answered by %s\n",
		len(out), humanSize(total), runtimeImagesEnv)
	return out, true, nil
}

func resolveInstallRuntimeImagesFor(instance string, promptOut io.Writer, answerIn io.Reader, imageTag string, forcePrompt bool) ([]runtimeImageChoice, error) {
	// THE MOST SPECIFIC SIGNAL WINS, and an environment answer is the most specific there is: an
	// installer set it for THIS run, having just asked the operator.
	if choices, answered, err := explicitRuntimeImages(promptOut, imageTag); err != nil {
		return nil, err
	} else if answered {
		return choices, nil
	}

	// NOBODY TO ASK.
	if !installIsInteractive() {
		if choices, answered := assumeYesRuntimeImages(promptOut, imageTag); answered {
			return choices, nil
		}
		fmt.Fprintln(promptOut, "  runtime images: not asking (non-interactive install) — fetch one later from the Runtime Images page if a run needs it")
		return nil, nil
	}
	if !forcePrompt {
		if p, err := runtimeImagesAnsweredPath(instance); err == nil {
			if b, rerr := os.ReadFile(p); rerr == nil {
				recorded := strings.TrimSpace(string(b))
				fmt.Fprintf(promptOut, "  runtime images: %s (already answered for %s — not asking again)\n", recorded, instance)
				if recorded == "" || recorded == "none" {
					return nil, nil
				}
				// A recorded selection is re-resolved against TODAY'S choice list rather than replayed verbatim:
				// the list can change between versions, and a stale index would fetch the wrong image silently.
				if idxs, err := parseRuntimeImageSelection(recorded, len(installRuntimeImageChoices(imageTag))); err == nil {
					choices := installRuntimeImageChoices(imageTag)
					out := make([]runtimeImageChoice, 0, len(idxs))
					for _, i := range idxs {
						out = append(out, choices[i])
					}
					return out, nil
				}
			}
		}
	}
	// --yes IS AN ANSWER, and it is checked AFTER the record so that it can never override one: an
	// instance that already answered "none" (or "1,3") keeps that answer.
	if choices, answered := assumeYesRuntimeImages(promptOut, imageTag); answered {
		return choices, nil
	}
	return resolveInstallRuntimeImages(promptOut, answerIn, imageTag)
}

// assumeYesRuntimeImages returns every choice when ORCHICON_ASSUME_YES names an affirmative answer.
func assumeYesRuntimeImages(promptOut io.Writer, imageTag string) ([]runtimeImageChoice, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ORCHICON_ASSUME_YES"))) {
	case "1", "true", "yes", "y":
	default:
		return nil, false
	}
	choices := installRuntimeImageChoices(imageTag)
	var total int64
	for _, c := range choices {
		total += c.SizeBytes
	}
	fmt.Fprintf(promptOut, "  runtime images: all %d selected (%s) — answered affirmatively (--yes)\n", len(choices), humanSize(total))
	return choices, true
}

// runtimeImagesAnsweredPath is where the answer is recorded, in the instance's own state dir.
func runtimeImagesAnsweredPath(instance string) (string, error) {
	dir, err := hostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runtimeImagesAnsweredFile), nil
}

// recordRuntimeImagesAnswer persists what the operator chose, so the next install does not re-ask. Best
// effort: losing the record costs one extra prompt, never a broken instance.
func recordRuntimeImagesAnswer(instance, answer string) {
	p, err := runtimeImagesAnsweredPath(instance)
	if err != nil {
		return
	}
	if strings.TrimSpace(answer) == "" {
		answer = "none"
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(strings.TrimSpace(answer)+"\n"), 0o644)
}

func resolveInstallRuntimeImages(promptOut io.Writer, answerIn io.Reader, imageTag string) ([]runtimeImageChoice, error) {
	choices := installRuntimeImageChoices(imageTag)
	fmt.Fprintln(promptOut, "")
	fmt.Fprintln(promptOut, "  Runtime images are OPTIONAL and fetched on demand. A project starts in local")
	fmt.Fprintln(promptOut, "  mode, so an install with no runtime image is complete; a run that needs a")
	fmt.Fprintln(promptOut, "  container asks for its image on the Runtime Images page.")
	fmt.Fprintln(promptOut, "")
	for i, c := range choices {
		fmt.Fprintf(promptOut, "    %d) %-28s %-10s %s\n", i+1, c.Label, humanSize(c.SizeBytes), c.Ref)
	}
	fmt.Fprint(promptOut, "  Download any runtime images now? [none] (e.g. 1,3 — or \"all\"): ")
	sc := bufio.NewScanner(answerIn)
	answer := ""
	if sc.Scan() {
		answer = sc.Text()
	}
	idxs, err := parseRuntimeImageSelection(answer, len(choices))
	if err != nil {
		fmt.Fprintf(promptOut, "  %v — downloading no runtime image\n", err)
		return nil, nil
	}
	out := make([]runtimeImageChoice, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, choices[i])
	}
	return out, nil
}

// runtimeImageTemplate is the shipped Dockerfile for a variant, as embedded in this binary.
func runtimeImageTemplate(variant string) (dockerfile string, baseImage string, err error) {
	file := map[string]string{
		"gui": "deploy/runtime/Dockerfile.gui",
		"dev": "deploy/runtime/Dockerfile.dev",
	}[variant]
	if file == "" {
		return "", "", fmt.Errorf("%q is not a locally-built variant", variant)
	}
	b, err := assets.RuntimeImageTemplatesFS.ReadFile(file)
	if err != nil {
		return "", "", fmt.Errorf("read the embedded %s template: %w", file, err)
	}
	return string(b), "ghcr.io/beardedparrott/orchicon-runtime:" + env("ORCHICON_IMAGE_TAG", "latest"), nil
}

// ensureRuntimeImageVariant makes a variant image present LOCALLY, by building it from its shipped
// template when it is not there. IT CANNOT PULL.
func ensureRuntimeImageVariant(variant, ref, buildContext string) error {
	if imagePresent(ref) {
		fmt.Printf("image %s is already built on this host\n", ref)
		return nil
	}
	dockerfile, baseImage, err := runtimeImageTemplate(variant)
	if err != nil {
		return err
	}
	// THE BASE MUST BE PRESENT FIRST, and this is structural: a variant's Dockerfile is `FROM
	// ${BASE_IMAGE}`.
	if !imagePresent(baseImage) {
		return fmt.Errorf("%s builds FROM %s, which is not on this host yet — fetch the base image first", ref, baseImage)
	}
	fmt.Printf("building %s from its shipped template (this takes a few minutes; it is not a download)\n", ref)
	// THE DOCKERFILE GOES IN ON STDIN, so nothing is written to disk and no path has to be plausible in a
	// temp directory. `-f -` is docker's documented "read the Dockerfile from stdin".
	cmd := engineCommand("build", "-f", "-", "-t", ref, "--build-arg", "BASE_IMAGE="+baseImage, buildContext)
	cmd.Stdin = strings.NewReader(dockerfile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s: %w", ref, err)
	}
	fmt.Printf("built %s\n", ref)
	return nil
}
// ensureInstallRuntimeImages downloads the chosen runtime images.
func ensureInstallRuntimeImages(choices []runtimeImageChoice, out io.Writer) {
	if len(choices) == 0 {
		fmt.Fprintln(out, "orchicon: no runtime image downloaded — runs needing a container will ask for one on the Runtime Images page")
		return
	}
	for _, c := range choices {
		verb := "fetching"
		if !c.Published {
			verb = "preparing"
		}
		fmt.Fprintf(out, "  %s %s (%s)...\n", verb, c.Ref, humanSize(c.SizeBytes))
		var ferr error
		if c.Published {
			ferr = ensureImage(c.Ref)
		} else {
			// BUILT, NOT PULLED. `podman pull orchicon-runtime:dev` fails — a short name with no registry
			// behind it — and nothing publishes it any more. The template is embedded in THIS binary.
			ferr = ensureRuntimeImageVariant(c.Variant, c.Ref, c.BuildContext)
		}
		if err := ferr; err != nil {
			fmt.Fprintf(out, "  warning: %v\n", err)
			fmt.Fprintf(out, "  (%s is optional — continuing; it can be fetched later from the Runtime Images page)\n", c.Variant)
		}
	}
}

// selectionIndexes renders a selection back into the "1,3" form the record stores, so a later install can
// re-resolve it against the CURRENT choice list rather than replaying stale structs.
func selectionIndexes(picked []runtimeImageChoice) string {
	all := installRuntimeImageChoices("")
	idx := make(map[string]int, len(all))
	for i, c := range all {
		idx[c.Variant] = i + 1
	}
	var parts []string
	for _, p := range picked {
		if n, ok := idx[p.Variant]; ok {
			parts = append(parts, strconv.Itoa(n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// runtimeImageBuildContext is the directory `docker build` runs from for an unpublished variant.
func runtimeImageBuildContext(variant string) string {
	dir := filepath.Join(os.TempDir(), "orchicon-runtime-build", variant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// A build with a context that does not exist fails inside the engine with a path error. Falling back
		// to the working directory keeps the build possible and the failure honest if even that is unusable.
		return "."
	}
	return dir
}
