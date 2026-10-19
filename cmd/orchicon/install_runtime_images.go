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
)

// runtimeImageChoice is one runtime image the install may fetch, with the size
// the prompt shows so the choice is informed.
type runtimeImageChoice struct {
	Variant string
	Ref     string
	Label   string
	// SizeBytes is the approximate download size, used ONLY to render the
	// prompt. The variants differ by an order of magnitude, so "do you want
	// runtime images" is not an answerable question without them.
	SizeBytes int64
}

// installRuntimeImageChoices lists the runtime images an install can offer,
// base first (every other variant derives FROM it).
//
// EVERY ENTRY HERE MUST EXIST IN THE REGISTRY. A variant is offered by TAG, so an entry whose tag the release
// pipeline never publishes is a pull that can only fail — and it failed loudly on every single install:
//
//	pull ghcr.io/beardedparrott/orchicon-runtime:web-research: manifest unknown
//	(web-research is optional — continuing; it can be fetched later from the Runtime Images page)
//
// release.yml publishes base, gui-<ver>/gui and dev-<ver>/dev and NOTHING else, and `scripts/runtime.sh build`
// builds exactly those three locally. "web-research" was offered anyway (and "fetched later" could never
// work either, because the tag does not exist). Removed rather than warned about: a warning on every install
// trains operators to ignore warnings, and this one named a failure that was never actionable.
func installRuntimeImageChoices(imageTag string) []runtimeImageChoice {
	return []runtimeImageChoice{
		{Variant: "base", Ref: "ghcr.io/beardedparrott/orchicon-runtime:" + imageTag, Label: "runtime base", SizeBytes: 1_000_000_000},
		{Variant: "gui", Ref: runtimeVariantRef("gui", imageTag), Label: "runtime GUI (+150 MB)", SizeBytes: 150_000_000},
		{Variant: "dev", Ref: runtimeVariantRef("dev", imageTag), Label: "runtime dev (+1.5-2.5 GB)", SizeBytes: 2_000_000_000},
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

// resolveInstallRuntimeImages asks which runtime images to download now and
// returns the chosen ones. DEFAULT NONE, and a non-interactive install takes
// the default rather than blocking: a project starts in local mode, so an
// install that needs no runtime image is a complete install.
//
// The prompt is written to promptOut (the caller's console) and the answer is
// read from answerIn.
// runtimeImagesAnsweredFile records that this instance has ALREADY been asked which runtime images to fetch.
//
// THE PROMPT USED TO APPEAR ON EVERY INSTALL, and an install runs on every `make instance-up` /
// `rebuild-<instance>`. So a developer who had answered it once — or declined, which is the recommended
// default — was asked again on every rebuild, alongside the port prompts. That is the difference between a
// first-run questionnaire and a command you cannot put in a script.
//
// The ANSWER is recorded, not just the fact of asking: "none" is recorded as "none", so declining stays
// declined, while a later explicit `orchicon install` with a real selection still overrides it. Same shape as
// ports.json and the residency record — per instance, written by the install, read back by the next one.
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
	// THE MOST SPECIFIC SIGNAL WINS, and an environment answer is the most specific
	// there is: an installer set it for THIS run, having just asked the operator. It
	// therefore outranks both the interactivity gate below (which would discard it)
	// and a recorded answer (which exists so a REBUILD does not re-ask, not to
	// overrule someone who has just answered).
	if choices, answered, err := explicitRuntimeImages(promptOut, imageTag); err != nil {
		return nil, err
	} else if answered {
		return choices, nil
	}

	// NOBODY TO ASK. A redirected/backgrounded install must not block on a question — the same rule the port
	// prompt follows (install_ports.go promptTerminal), and for the same reason: the read would either hang
	// or consume input that belongs to whatever is driving us.
	//
	// --yes STILL COUNTS HERE. It is an answer that never needed a terminal, and it is the documented way to
	// make a non-interactive install decide something: `scripts/install.sh --yes` exports ORCHICON_ASSUME_YES
	// and runs `orchicon install` with no flags, so the environment is the only channel it has.
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
				// A recorded selection is re-resolved against TODAY'S choice list rather than replayed
				// verbatim: the list can change between versions, and a stale index would fetch the wrong
				// image silently.
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
	// --yes IS AN ANSWER, and it is checked AFTER the record so that it can never override one: an instance
	// that already answered "none" (or "1,3") keeps that answer, because a rebuild is not a first install and
	// --yes must not turn every rebuild into a multi-GB download. On a first install, "all" is what an
	// affirmative answer to "which of these?" means, and the total is printed BEFORE anything is fetched so a
	// several-GB choice is never a surprise.
	if choices, answered := assumeYesRuntimeImages(promptOut, imageTag); answered {
		return choices, nil
	}
	return resolveInstallRuntimeImages(promptOut, answerIn, imageTag)
}

// assumeYesRuntimeImages returns every choice when ORCHICON_ASSUME_YES names an affirmative answer. The
// second return says whether an answer was given AT ALL, so a caller can distinguish "--yes answered" from
// "no opinion" without re-reading the environment.
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

// ensureInstallRuntimeImages downloads the chosen runtime images. Failures
// WARN per image and never fail the install: a declined or failed image leaves
// a fully working plane in local mode, which is exactly the state an install
// that fetched nothing already has.
func ensureInstallRuntimeImages(choices []runtimeImageChoice, out io.Writer) {
	if len(choices) == 0 {
		fmt.Fprintln(out, "orchicon: no runtime image downloaded — runs needing a container will ask for one on the Runtime Images page")
		return
	}
	for _, c := range choices {
		fmt.Fprintf(out, "  fetching %s (%s)...\n", c.Ref, humanSize(c.SizeBytes))
		if err := ensureImage(c.Ref); err != nil {
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
