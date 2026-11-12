package diffs

// MAX_EMPHASIS caps the cheap word-level emphasis path. Beyond it the whole
// changed middle is one span (the GUI's fallback). Must match sideBySide.ts.
const MAX_EMPHASIS = 256

// EmphasizeTokens computes byte-range emphasis spans for a changed line pair.
func EmphasizeTokens(oldText, newText string) (oldSpans, newSpans []EmphasisSpan) {
	oldTrimStart, newTrimStart := trimCommonPrefix(oldText, newText)
	oldTrimEnd, newTrimEnd := trimCommonSuffix(oldText, newText, oldTrimStart, newTrimStart)

	oldLen := oldTrimEnd - oldTrimStart
	newLen := newTrimEnd - newTrimStart
	if oldLen+newLen > MAX_EMPHASIS {
		// Cheap fallback: whole changed middle is one span per side.
		if oldLen > 0 {
			oldSpans = append(oldSpans, EmphasisSpan{Start: oldTrimStart, End: oldTrimEnd, Type: "del"})
		}
		if newLen > 0 {
			newSpans = append(newSpans, EmphasisSpan{Start: newTrimStart, End: newTrimEnd, Type: "add"})
		}
		return oldSpans, newSpans
	}

	if oldLen > 0 {
		oldSpans = append(oldSpans, EmphasisSpan{Start: oldTrimStart, End: oldTrimEnd, Type: "del"})
	}
	if newLen > 0 {
		newSpans = append(newSpans, EmphasisSpan{Start: newTrimStart, End: newTrimEnd, Type: "add"})
	}
	return oldSpans, newSpans
}

// trimCommonPrefix returns the byte index of the first differing character
// in both strings (0-based). Mirrors sideBySide.ts trimCommonPrefix.
func trimCommonPrefix(a, b string) (oldIndex, newIndex int) {
	i := 0
	min := len(a)
	if len(b) < min {
		min = len(b)
	}
	for i < min && a[i] == b[i] {
		i++
	}
	return i, i
}

// trimCommonSuffix returns the byte index one past the last differing character in both strings,
// given the common-prefix 0-based index.
func trimCommonSuffix(a, b string, prefixOld, prefixNew int) (oldEnd, newEnd int) {
	i := len(a)
	j := len(b)
	min := len(a)
	if len(b) < min {
		min = len(b)
	}
	prefixMax := prefixOld
	if prefixNew > prefixMax {
		prefixMax = prefixNew
	}
	trimmed := 0
	for trimmed < min-prefixMax && a[i-1] == b[j-1] {
		i--
		j--
		trimmed++
	}
	return i, j
}
