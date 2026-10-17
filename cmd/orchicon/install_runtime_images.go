package main

import (
	"bufio"
	"fmt"
	"io"
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
func installRuntimeImageChoices(imageTag string) []runtimeImageChoice {
	return []runtimeImageChoice{
		{Variant: "base", Ref: "ghcr.io/beardedparrott/orchicon-runtime:" + imageTag, Label: "runtime base", SizeBytes: 1_000_000_000},
		{Variant: "gui", Ref: runtimeVariantRef("gui", imageTag), Label: "runtime GUI (+150 MB)", SizeBytes: 150_000_000},
		{Variant: "dev", Ref: runtimeVariantRef("dev", imageTag), Label: "runtime dev (+1.5-2.5 GB)", SizeBytes: 2_000_000_000},
		{Variant: "web-research", Ref: runtimeVariantRef("web-research", imageTag), Label: "runtime web-research (+300 MB)", SizeBytes: 300_000_000},
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
